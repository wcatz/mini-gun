package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	var (
		tps         = flag.Float64("tps", 1, "target transactions per second")
		count       = flag.Int("count", 0, "total txs to fire (0 = unlimited)")
		amount      = flag.Int64("amount", 2_000_000, "lovelace per tx (default: 2 ADA)")
		submitURL   = flag.String("submit-url", "http://localhost:8091", "tx-submit-api URL")
		ogmiosURL   = flag.String("ogmios-url", "ws://localhost:1337", "ogmios WebSocket URL")
		walletASkey = flag.String("wallet-a-skey", "", "path to wallet A signing key (required)")
		walletAAddr = flag.String("wallet-a-addr", "", "wallet A bech32 address (required)")
		walletBSkey = flag.String("wallet-b-skey", "", "path to wallet B signing key (required)")
		walletBAddr = flag.String("wallet-b-addr", "", "wallet B bech32 address (required)")
		utxoOffset       = flag.Int("utxo-offset", 0, "UTXO selection offset (0=largest, 1=2nd largest, etc.)")
		padding          = flag.Int("padding", 0, "bytes of random metadata to pad each tx (0=none)")
		split            = flag.Int("split", 0, "split largest UTXO into N equal outputs, then exit")
		plutusLock       = flag.Uint64("plutus-lock", 0, "lock N lovelace at always-succeeds script address, then exit")
		plutusUnlockTx   = flag.String("plutus-unlock-txhash", "", "txhash of locked script UTXO to spend")
		plutusUnlockIdx  = flag.Uint("plutus-unlock-idx", 0, "output index of locked script UTXO")
		plutusUnlockVal  = flag.Uint64("plutus-unlock-value", 0, "lovelace value of locked script UTXO")
		plutusCollTx     = flag.String("plutus-collateral-txhash", "", "txhash of collateral UTXO (wallet A)")
		plutusCollIdx    = flag.Uint("plutus-collateral-idx", 0, "output index of collateral UTXO")
	)
	flag.Parse()

	if *walletASkey == "" || *walletAAddr == "" || *walletBSkey == "" || *walletBAddr == "" {
		fmt.Fprintln(os.Stderr, "error: all wallet flags are required")
		flag.Usage()
		os.Exit(1)
	}

	walletA, err := LoadWallet(*walletASkey, *walletAAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load wallet A: %v\n", err)
		os.Exit(1)
	}

	walletB, err := LoadWallet(*walletBSkey, *walletBAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load wallet B: %v\n", err)
		os.Exit(1)
	}

	ogmios, err := NewOgmiosClient(*ogmiosURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect ogmios: %v\n", err)
		os.Exit(1)
	}
	defer ogmios.Close()

	params, err := ogmios.QueryProtocolParameters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "query protocol params: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("protocol params: minFeeCoefficient=%d, minFeeConstant=%d lovelace\n",
		params.MinFeeCoefficient, params.MinFeeConstant.Ada.Lovelace)

	gun := &Gun{
		TPS:          *tps,
		Count:        *count,
		Amount:       uint64(*amount),
		SubmitURL:    *submitURL,
		WalletA:      walletA,
		WalletB:      walletB,
		Ogmios:       ogmios,
		Params:       params,
		UTxOOffset:   *utxoOffset,
		PaddingBytes: *padding,
	}

	if *split > 0 {
		if err := gun.Split(*split); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *plutusLock > 0 {
		if err := gun.PlutusLock(*plutusLock); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *plutusUnlockTx != "" {
		if *plutusCollTx == "" {
			fmt.Fprintln(os.Stderr, "error: --plutus-collateral-txhash required with --plutus-unlock-txhash")
			os.Exit(1)
		}
		if *plutusUnlockVal == 0 {
			fmt.Fprintln(os.Stderr, "error: --plutus-unlock-value required")
			os.Exit(1)
		}
		err := gun.PlutusUnlock(
			*plutusUnlockTx, uint32(*plutusUnlockIdx), *plutusUnlockVal,
			*plutusCollTx, uint32(*plutusCollIdx),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := gun.Fire(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
