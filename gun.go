package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Gun orchestrates the ping-pong transaction loop.
type Gun struct {
	TPS       float64
	Count     int
	Amount    uint64
	SubmitURL string
	WalletA   *Wallet
	WalletB   *Wallet
	Ogmios    *OgmiosClient
	Params    *ProtocolParams

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

	bestA := pickLargestUTxO(utxosA)
	bestB := pickLargestUTxO(utxosB)

	currentA := TxInput{TxHash: mustParseHash(bestA.TxHash), Index: bestA.Index}
	currentAValue := bestA.Value
	currentB := TxInput{TxHash: mustParseHash(bestB.TxHash), Index: bestB.Index}
	currentBValue := bestB.Value

	fmt.Printf("mini-gun firing at %.1f TPS -> %s\n", g.TPS, g.SubmitURL)
	fmt.Printf("  wallet A: %s (%.6f ADA)\n", shortAddr(g.WalletA.AddrBech32), float64(currentAValue)/1_000_000)
	fmt.Printf("  wallet B: %s (%.6f ADA)\n", shortAddr(g.WalletB.AddrBech32), float64(currentBValue)/1_000_000)
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
		tx, err := BuildAndSign(dir.sender, dir.receiver, g.Amount, *input, *inputValue, g.Params, ttl)
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

func pickLargestUTxO(utxos []UTxO) UTxO {
	best := utxos[0]
	for _, u := range utxos[1:] {
		if u.Value > best.Value {
			best = u
		}
	}
	return best
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
