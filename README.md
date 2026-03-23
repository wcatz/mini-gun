# mini-gun

Cardano transaction generator for testnet load testing and Plutus script validation.

Builds and submits transactions via Ogmios (WebSocket) and tx-submit-api (HTTP).
Manual CBOR construction — no external Cardano SDK dependencies.

## Requirements

- Go 1.24+
- Ogmios v6 instance
- tx-submit-api endpoint (Haskell node, Dingo, or Amaru)
- Two funded wallets on target network

## Build

```
go build -o mini-gun .
```

## Usage

### Ping-pong mode (sustained TPS)

```
./mini-gun \
  --wallet-a-skey wallets/a.skey --wallet-a-addr addr_test1v... \
  --wallet-b-skey wallets/b.skey --wallet-b-addr addr_test1v... \
  --ogmios-url ws://localhost:1337 \
  --submit-url http://localhost:8090 \
  --tps 1 --count 100
```

Alternates A->B and B->A transactions, chaining change outputs for sustained throughput
without waiting for on-chain confirmation.

### Split UTXOs (parallel instances)

```
./mini-gun --split 4 \
  --wallet-a-skey wallets/a.skey --wallet-a-addr addr_test1v... \
  --wallet-b-skey wallets/b.skey --wallet-b-addr addr_test1v...
```

Fans largest UTXO into N equal outputs. Run multiple instances with `--utxo-offset 0..N-1`.

### Plutus lock (send ADA to always-succeeds script)

```
./mini-gun --plutus-lock 5000000 \
  --wallet-a-skey wallets/a.skey --wallet-a-addr addr_test1v... \
  --wallet-b-skey wallets/b.skey --wallet-b-addr addr_test1v...
```

### Plutus unlock (spend from script address)

```
./mini-gun \
  --plutus-unlock-txhash <lock-tx-hash> \
  --plutus-unlock-idx 0 \
  --plutus-unlock-value 5000000 \
  --plutus-collateral-txhash <collateral-utxo-hash> \
  --plutus-collateral-idx 1 \
  --wallet-a-skey wallets/a.skey --wallet-a-addr addr_test1v... \
  --wallet-b-skey wallets/b.skey --wallet-b-addr addr_test1v...
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--tps` | 1 | Target transactions per second |
| `--count` | 0 | Total transactions (0 = unlimited) |
| `--amount` | 2000000 | Lovelace per transaction |
| `--submit-url` | http://localhost:8091 | tx-submit-api URL |
| `--ogmios-url` | ws://localhost:1337 | Ogmios WebSocket URL |
| `--wallet-a-skey` | | Wallet A signing key path (required) |
| `--wallet-a-addr` | | Wallet A bech32 address (required) |
| `--wallet-b-skey` | | Wallet B signing key path (required) |
| `--wallet-b-addr` | | Wallet B bech32 address (required) |
| `--utxo-offset` | 0 | UTXO selection offset for parallel instances |
| `--padding` | 0 | Bytes of random metadata padding per tx |
| `--split` | 0 | Split largest UTXO into N outputs |
| `--plutus-lock` | 0 | Lock N lovelace at always-succeeds script |
| `--plutus-unlock-txhash` | | Script UTXO tx hash to spend |
| `--plutus-unlock-idx` | 0 | Script UTXO output index |
| `--plutus-unlock-value` | 0 | Script UTXO lovelace value |
| `--plutus-collateral-txhash` | | Collateral UTXO tx hash |
| `--plutus-collateral-idx` | 0 | Collateral UTXO output index |

## Transaction types

**Standard payment** — Babbage-era, tag 258 input sets, two-pass fee estimation.

**Plutus lock** — Sends ADA to always-succeeds PlutusV2 script address with inline
unit datum. Conway-era Babbage-format map outputs.

**Plutus unlock** — Spends script UTXO with Conway redeemer map, script witness,
collateral input/return, and `script_integrity_hash`. Includes script execution
fee via exact rational arithmetic.

## Architecture

All transaction building is manual CBOR via `fxamacker/cbor/v2`. No gouroboros
transaction types or Cardano SDK. This gives byte-level control over the wire
format — important for testing node validation edge cases.

## Query tool

```
go run cmd/query/main.go [ogmios-url]
```

Dumps UTXO state for the configured wallet addresses.
