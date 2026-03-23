package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Gun orchestrates the ping-pong transaction loop.
type Gun struct {
	TPS          float64
	Count        int
	Amount       uint64
	SubmitURL    string
	WalletA      *Wallet
	WalletB      *Wallet
	Ogmios       *OgmiosClient
	Params       *ProtocolParams
	UTxOOffset   int
	PaddingBytes int

	total   atomic.Int64
	success atomic.Int64
	failed  atomic.Int64
	totalMs atomic.Int64
}

type direction struct {
	sender   *Wallet
	receiver *Wallet
	label    string
}

// Fire runs the rate-limited ping-pong transaction loop.
func (g *Gun) Fire(ctx context.Context) error {
	slot, err := g.Ogmios.QueryTip()
	if err != nil {
		return fmt.Errorf("query tip: %w", err)
	}
	ttl := slot + 7200 // ~2 hours of slots

	utxosA, err := g.Ogmios.QueryUTxOs(g.WalletA.AddrBech32)
	if err != nil {
		return fmt.Errorf("query utxos A: %w", err)
	}
	utxosB, err := g.Ogmios.QueryUTxOs(g.WalletB.AddrBech32)
	if err != nil {
		return fmt.Errorf("query utxos B: %w", err)
	}

	if len(utxosA) == 0 {
		return fmt.Errorf("wallet A has no UTXOs")
	}
	if len(utxosB) == 0 {
		return fmt.Errorf("wallet B has no UTXOs")
	}

	bestA := pickUTxOByOffset(utxosA, g.UTxOOffset)
	bestB := pickUTxOByOffset(utxosB, g.UTxOOffset)

	currentA := TxInput{TxHash: mustParseHash(bestA.TxHash), Index: bestA.Index}
	currentAValue := bestA.Value
	currentB := TxInput{TxHash: mustParseHash(bestB.TxHash), Index: bestB.Index}
	currentBValue := bestB.Value

	fmt.Printf("mini-gun firing at %.1f TPS -> %s\n", g.TPS, g.SubmitURL)
	fmt.Printf("  wallet A: %s (%.6f ADA) utxo-offset=%d/%d\n",
		shortAddr(g.WalletA.AddrBech32), float64(currentAValue)/1_000_000,
		g.UTxOOffset, len(utxosA))
	fmt.Printf("  wallet B: %s (%.6f ADA) utxo-offset=%d/%d\n",
		shortAddr(g.WalletB.AddrBech32), float64(currentBValue)/1_000_000,
		g.UTxOOffset, len(utxosB))
	fmt.Println()

	limiter := rate.NewLimiter(rate.Limit(g.TPS), 1)
	startTime := time.Now()

	dirs := [2]direction{
		{sender: g.WalletA, receiver: g.WalletB, label: "A->B"},
		{sender: g.WalletB, receiver: g.WalletA, label: "B->A"},
	}

	for i := 0; g.Count == 0 || int(g.total.Load()) < g.Count; i++ {
		if err := limiter.Wait(ctx); err != nil {
			break // context cancelled (Ctrl+C)
		}

		dir := dirs[i%2]
		var input *TxInput
		var inputValue *uint64

		if i%2 == 0 {
			input = &currentA
			inputValue = &currentAValue
		} else {
			input = &currentB
			inputValue = &currentBValue
		}

		txStart := time.Now()
		tx, err := BuildAndSign(dir.sender, dir.receiver, g.Amount, *input, *inputValue, g.Params, ttl, g.PaddingBytes)
		if err != nil {
			g.total.Add(1)
			g.failed.Add(1)
			fmt.Printf("  [%s] tx %-4d %s  %d lovelace  BUILD ERROR: %v\n",
				time.Now().Format("15:04:05"), g.total.Load(), dir.label, g.Amount, err)
			continue
		}

		err = SubmitTx(g.SubmitURL, tx.CborBytes)
		elapsed := time.Since(txStart)
		g.total.Add(1)
		g.totalMs.Add(elapsed.Milliseconds())

		txHashHex := hex.EncodeToString(tx.TxHash[:])

		if err != nil {
			g.failed.Add(1)
			fmt.Printf("  [%s] tx %-4d %s  %d lovelace  hash:%s  x  (%dms) %v\n",
				time.Now().Format("15:04:05"), g.total.Load(), dir.label,
				g.Amount, txHashHex[:12], elapsed.Milliseconds(), err)
		} else {
			g.success.Add(1)
			fmt.Printf("  [%s] tx %-4d %s  %d lovelace  hash:%s  ok  (%dms)\n",
				time.Now().Format("15:04:05"), g.total.Load(), dir.label,
				g.Amount, txHashHex[:12], elapsed.Milliseconds())

			// Chain: use change output (index 1) as next input for this wallet
			changeValue := *inputValue - g.Amount - tx.Fee
			*input = TxInput{TxHash: tx.TxHash[:], Index: 1}
			*inputValue = changeValue
		}

		// Refresh TTL every 100 txs
		if g.total.Load()%100 == 0 {
			if newSlot, err := g.Ogmios.QueryTip(); err == nil {
				ttl = newSlot + 7200
			}
		}
	}

	g.printStats(startTime)
	return nil
}

func (g *Gun) printStats(startTime time.Time) {
	elapsed := time.Since(startTime)
	total := g.total.Load()
	succ := g.success.Load()
	fail := g.failed.Load()

	var avgMs int64
	if total > 0 {
		avgMs = g.totalMs.Load() / total
	}

	var actualTPS float64
	if elapsed.Seconds() > 0 {
		actualTPS = float64(total) / elapsed.Seconds()
	}

	fmt.Println()
	fmt.Println("  === Stats ===")
	fmt.Printf("  Total: %d | Success: %d | Failed: %d | Avg: %dms | Actual TPS: %.1f\n",
		total, succ, fail, avgMs, actualTPS)
}

// PlutusLock sends ADA from wallet A to the always-succeeds V2 script address.
// It prints the script UTXO (txhash#index) for use with PlutusUnlock.
func (g *Gun) PlutusLock(lockAmount uint64) error {
	slot, err := g.Ogmios.QueryTip()
	if err != nil {
		return fmt.Errorf("query tip: %w", err)
	}
	ttl := slot + 7200

	utxos, err := g.Ogmios.QueryUTxOs(g.WalletA.AddrBech32)
	if err != nil {
		return fmt.Errorf("query utxos A: %w", err)
	}
	if len(utxos) == 0 {
		return fmt.Errorf("wallet A has no UTXOs")
	}

	best := pickUTxOByOffset(utxos, g.UTxOOffset)
	input := TxInput{TxHash: mustParseHash(best.TxHash), Index: best.Index}

	fmt.Printf("plutus lock: %s (%.6f ADA) -> always-succeeds script\n",
		shortAddr(g.WalletA.AddrBech32), float64(best.Value)/1_000_000)

	tx, err := BuildScriptLockTx(PlutusLockParams{
		Sender:     g.WalletA,
		Input:      input,
		InputValue: best.Value,
		LockAmount: lockAmount,
		Params:     g.Params,
		TTL:        ttl,
	})
	if err != nil {
		return fmt.Errorf("build lock tx: %w", err)
	}

	if err := SubmitTx(g.SubmitURL, tx.CborBytes); err != nil {
		return fmt.Errorf("submit lock tx: %w", err)
	}

	txHashHex := hex.EncodeToString(tx.TxHash[:])
	scriptHash := hex.EncodeToString(AlwaysSucceedsScriptHash())
	fmt.Printf("  lock tx:     %s\n", txHashHex)
	fmt.Printf("  script hash: %s\n", scriptHash)
	fmt.Printf("  script utxo: %s#0  (%.6f ADA with inline unit datum)\n",
		txHashHex, float64(lockAmount)/1_000_000)
	fmt.Printf("  fee:         %d lovelace\n", tx.Fee)
	fmt.Println("\nwait for confirmation then run --plutus-unlock with the script utxo above")
	return nil
}

// PlutusUnlock spends a script UTXO using wallet A as collateral provider.
// scriptTxHash and scriptIdx identify the UTXO at the always-succeeds address.
// collateralTxHash and collateralIdx identify a vkey-locked UTXO for collateral.
func (g *Gun) PlutusUnlock(
	scriptTxHash string, scriptIdx uint32, scriptValue uint64,
	collateralTxHash string, collateralIdx uint32,
) error {
	slot, err := g.Ogmios.QueryTip()
	if err != nil {
		return fmt.Errorf("query tip: %w", err)
	}
	ttl := slot + 7200

	// Fetch collateral value from chain if not provided via queryUTxOs.
	utxos, err := g.Ogmios.QueryUTxOs(g.WalletA.AddrBech32)
	if err != nil {
		return fmt.Errorf("query utxos A: %w", err)
	}
	var collateralValue uint64
	for _, u := range utxos {
		if u.TxHash == collateralTxHash && u.Index == collateralIdx {
			collateralValue = u.Value
			break
		}
	}
	if collateralValue == 0 {
		return fmt.Errorf("collateral utxo %s#%d not found in wallet A", collateralTxHash, collateralIdx)
	}

	scriptInput := TxInput{TxHash: mustParseHash(scriptTxHash), Index: scriptIdx}
	collateralInput := TxInput{TxHash: mustParseHash(collateralTxHash), Index: collateralIdx}

	fmt.Printf("plutus unlock: script utxo %s#%d (%.6f ADA)\n",
		scriptTxHash[:12], scriptIdx, float64(scriptValue)/1_000_000)
	fmt.Printf("  collateral:  %s#%d (%.6f ADA)\n",
		collateralTxHash[:12], collateralIdx, float64(collateralValue)/1_000_000)

	tx, err := BuildScriptUnlockTx(PlutusUnlockParams{
		Spender:         g.WalletA,
		ScriptInput:     scriptInput,
		ScriptValue:     scriptValue,
		CollateralInput: collateralInput,
		CollateralValue: collateralValue,
		Params:          g.Params,
		TTL:             ttl,
	})
	if err != nil {
		return fmt.Errorf("build unlock tx: %w", err)
	}

	if err := SubmitTx(g.SubmitURL, tx.CborBytes); err != nil {
		return fmt.Errorf("submit unlock tx: %w", err)
	}

	txHashHex := hex.EncodeToString(tx.TxHash[:])
	fmt.Printf("  unlock tx: %s\n", txHashHex)
	fmt.Printf("  fee:       %d lovelace\n", tx.Fee)
	return nil
}

// Split takes the largest UTXO from each wallet and fans it out into N equal outputs.
func (g *Gun) Split(numOutputs int) error {
	ogmios := g.Ogmios

	slot, err := ogmios.QueryTip()
	if err != nil {
		return fmt.Errorf("query tip: %w", err)
	}
	ttl := slot + 7200

	for _, w := range []*Wallet{g.WalletA, g.WalletB} {
		label := "A"
		if w == g.WalletB {
			label = "B"
		}

		utxos, err := ogmios.QueryUTxOs(w.AddrBech32)
		if err != nil {
			return fmt.Errorf("query utxos %s: %w", label, err)
		}
		if len(utxos) == 0 {
			return fmt.Errorf("wallet %s has no UTXOs", label)
		}

		best := pickUTxOByOffset(utxos, 0)
		fmt.Printf("wallet %s: splitting %s (%.6f ADA) into %d outputs\n",
			label, shortAddr(w.AddrBech32), float64(best.Value)/1_000_000, numOutputs)

		input := TxInput{TxHash: mustParseHash(best.TxHash), Index: best.Index}
		tx, err := BuildSplitTx(w, input, best.Value, numOutputs, g.Params, ttl)
		if err != nil {
			return fmt.Errorf("build split tx %s: %w", label, err)
		}

		err = SubmitTx(g.SubmitURL, tx.CborBytes)
		if err != nil {
			return fmt.Errorf("submit split tx %s: %w", label, err)
		}

		txHashHex := hex.EncodeToString(tx.TxHash[:])
		remaining := best.Value - tx.Fee
		perOutput := remaining / uint64(numOutputs)
		lastOutput := remaining - perOutput*uint64(numOutputs-1)
		fmt.Printf("  tx: %s  fee: %d  per-output: %.6f ADA  last: %.6f ADA\n",
			txHashHex[:16], tx.Fee,
			float64(perOutput)/1_000_000, float64(lastOutput)/1_000_000)
	}

	fmt.Println("\nsplit complete — wait for confirmation then run with --utxo-offset 0..N")
	return nil
}

// pickUTxOByOffset sorts UTXOs by value descending and returns the one
// at the given offset. Offset 0 = largest, 1 = second largest, etc.
// Clamps to the last UTXO if offset exceeds available count.
func pickUTxOByOffset(utxos []UTxO, offset int) UTxO {
	sorted := make([]UTxO, len(utxos))
	copy(sorted, utxos)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Value > sorted[j].Value
	})
	if offset >= len(sorted) {
		offset = len(sorted) - 1
	}
	return sorted[offset]
}

func mustParseHash(hashHex string) []byte {
	b, err := ParseTxHash(hashHex)
	if err != nil {
		panic(err)
	}
	return b
}

func shortAddr(addr string) string {
	if len(addr) > 20 {
		return addr[:12] + "..." + addr[len(addr)-6:]
	}
	return addr
}
