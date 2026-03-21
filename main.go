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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	gun := &Gun{
		TPS:       *tps,
		Count:     *count,
		Amount:    uint64(*amount),
		SubmitURL: *submitURL,
		WalletA:   walletA,
		WalletB:   walletB,
		Ogmios:    ogmios,
		Params:    params,
	}

	if err := gun.Fire(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
