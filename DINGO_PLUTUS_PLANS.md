# Dingo txpump Plutus Upgrade Plans

Research from cross-referencing mini-gun, dingo txpump, gouroboros Phase-1 rules,
and Amaru conformance tests. Date: 2026-03-23.

## Current State

Dingo's antithesis txpump (`internal/test/antithesis/internal/txpump/plutus.go`)
is a structural fuzzer — fires malformed CBOR to test parsing/routing, not validation.

### Known bugs in txpump Plutus builders

| Issue | Current | Correct |
|-------|---------|---------|
| Lock tx witnesses | Empty `map[any]any{}` | VKey signed |
| Datum hash algo | SHA-256 (line 67) | Blake2b-256, or inline datum |
| Datum style | Hash-based `[0, hash]` | Inline `[1, #6.24(cbor)]` (Babbage+) |
| Unlock witness key for V2 script | Key 3 (that's V1) | Key 6 |
| `script_integrity_hash` (body key 11) | Missing | Required |
| Collateral inputs (body key 13) | Missing | Required |
| Collateral return (body key 16) | Missing | Required |
| Total collateral (body key 17) | Missing | Required |
| TTL (body key 3) | Missing | Required |
| Fee calculation | Hardcoded 200k, no script fee | Base + script execution fee |

### Validation gaps in Dingo/Amaru

Neither Dingo (gouroboros) nor Amaru currently validate `PPViewHashesDontMatch`
(the `script_integrity_hash` check). Both mark this as a known conformance failure.
Only the Haskell node enforces this in Phase-1.

gouroboros `MinFeeTx` also omits the script execution fee component — it only
computes `minFeeA * (txSize-1) + minFeeB`.

---

## Option A: Structural Fix (txpump only)

**Goal**: Produce well-formed Plutus CBOR that correctly exercises Phase-1 parsing
paths. Transactions still get rejected at signature validation but the structure
is Conway-correct.

**Scope**: `plutus.go` only, no changes to other builders or infra.

### Steps

1. **Fix V2 script witness key**
   - `witnessWithScript.PlutusV2Scripts`: change `cbor:"3,keyasint"` → `cbor:"6,keyasint"`

2. **Switch lock tx to inline datum**
   - Replace `scriptOutputDatumHash` with Babbage map output using `DatumOption: [1, #6.24(datum_cbor)]`
   - Datum value: `d87980` (Constr 0 [] = unit), wrapped in tag 24
   - Drop `datumHash()` function (SHA-256 no longer needed)

3. **Add missing fields to unlock tx body**
   - New struct `txBodyUnlockFull` with keys:
     - 0: inputs (existing)
     - 1: outputs (existing)
     - 2: fee (existing)
     - 3: TTL — use synthetic slot from pump's epoch calc + buffer
     - 11: `script_integrity_hash` — hardcode a plausible 32-byte hash OR compute correctly (see below)
     - 13: collateral inputs (tag 258 set) — reuse one of the regular inputs or a dedicated collateral UTxO
     - 16: collateral return output
     - 17: total collateral (uint64)

4. **Compute `script_integrity_hash` correctly** (optional for Option A)
   - Preimage: `redeemers_cbor || langviews_cbor` (no witness datums with inline datum)
   - Redeemers: `0xa1 [0,0] [d87980, [mem, steps]]` (Conway map format)
   - Lang views: `0xa1 0x01 <cost_model_array_cbor>`
   - If no cost model available: use a zero-filled 32-byte hash as placeholder
   - Hash: blake2b-256 of preimage

5. **Add collateral handling to wallet**
   - `SelectCoins` already returns UTxOs — pick one extra for collateral
   - Or: reserve a dedicated collateral UTxO at genesis seeding

6. **Wire TTL through pump**
   - Pump already calculates synthetic slot (`pump.go` line 113-119)
   - Pass `currentSlot + 7200` as TTL to Plutus builders

### Files touched
- `plutus.go` — bulk of changes
- `pump.go` — pass TTL to Plutus submit functions

### What this gets you
- Structurally valid Conway Plutus CBOR
- Proper parsing coverage for script_integrity_hash, collateral, inline datums
- Still rejected at signature validation (no signing keys)
- Antithesis can test that malformed variants of these fields cause correct error responses

### What this does NOT get you
- Actual Phase-1 pass (no signatures)
- Phase-2 script execution coverage
- Dynamic fee calculation

---

## Option B: Full Plutus Validation (signing + params)

**Goal**: Produce transactions that pass Phase-1 and Phase-2 on devnet. Actually
exercise the Plutus validation and script execution path end-to-end in Antithesis.

**Scope**: txpump-wide changes to add signing, protocol params, and proper fee calc.
All existing builders benefit, not just Plutus.

### Steps

#### Phase 1: Key management

1. **Load genesis signing keys**
   - Devnet configurator already outputs genesis UTxO keys to `/configs/utxo-keys/`
   - Add `TXPUMP_SIGNING_KEY_FILE` env var to `config.go`
   - Parse Cardano text-envelope JSON (type `PaymentSigningKeyShelley_ed25519`)
   - Extract 32-byte ed25519 seed, derive signing key + verify key
   - Store in wallet alongside UTxOs

2. **Derive real addresses**
   - Compute payment credential: blake2b-224 of verify key
   - Build enterprise address: `0x60 | keyhash` (testnet)
   - Replace `deterministicAddr()` with real address derivation
   - All builders use wallet's real address for change outputs

3. **Add VKey witness to all builders**
   - Sign blake2b-256(body_bytes) with ed25519 signing key
   - Witness set key 0: `[[vkey, signature]]`
   - Refactor: extract `signAndWitness(bodyBytes, signingKey) -> witnessSet` helper
   - Wire through payment, delegation, governance, AND plutus builders

#### Phase 2: Protocol parameters

4. **Add local-state-query via gouroboros**
   - gouroboros supports `localstatequery` mini-protocol
   - Query `protocolParameters` at connection time (once, cache result)
   - Or: load from genesis shelley config file (devnet params are static)
   - Need: `MinFeeA`, `MinFeeB`, `ScriptExecutionPrices`, `CollateralPercentage`, `CostModels`

5. **Dynamic fee calculation for all builders**
   - Replace hardcoded `MinFee = 200_000` with two-pass estimation:
     - Pass 1: estimate size, compute `minFeeA * size + minFeeB`
     - Pass 2: rebuild with actual fee, recompute if size changed
   - For Plutus: add `ceil(pricesMem * mem + pricesSteps * steps)` using `math/big`

#### Phase 3: Plutus-specific

6. **Port mini-gun Plutus primitives**
   - `AlwaysSucceedsScript()` / `AlwaysSucceedsScriptHash()` — already identical hex
   - `ScriptEnterpriseAddress()` — 0x70 testnet script address
   - `inlineDatumBytes()` — `[1, #6.24(datum_cbor)]`
   - `buildRedeemersCbor()` — Conway map `{[0,0]: [d87980, [mem, steps]]}`
   - `buildLangViewsCbor()` — `{0x01: cost_model_array}`
   - `calculateScriptFee()` — `big.Rat` arithmetic
   - `script_integrity_hash` preimage assembly

7. **Rebuild lock tx**
   - Proper Babbage map-encoded output with inline datum
   - VKey witness
   - TTL
   - Two-pass fee estimation

8. **Rebuild unlock tx**
   - All 6 missing body fields (keys 3, 11, 13, 16, 17 + fix key 6)
   - VKey witness (spending collateral requires signer)
   - Conway redeemer map in witness set (key 5)
   - V2 script in witness set (key 6)
   - Correct `script_integrity_hash`

9. **UTxO tracking for Plutus round-trip**
   - Lock tx produces: script UTxO (index 0) + change UTxO (index 1)
   - Wallet tracks script UTxO separately (different address, needs unlock not spend)
   - Unlock tx consumes: script UTxO + collateral UTxO from wallet
   - Add `ScriptUTxOs []UTxO` field to Wallet
   - `submitPlutusLock` adds to `ScriptUTxOs`; `submitPlutusUnlock` consumes from it

10. **Epoch gating for lock-before-unlock**
    - Lock must happen before unlock (need confirmed script UTxO)
    - Current epoch gating enables both at epoch >= 3
    - Add state: only attempt unlock when `len(wallet.ScriptUTxOs) > 0`
    - Consider: lock in epoch 3, unlock in epoch 4+ (gives time for confirmation)

### Files touched
- `config.go` — new env vars for signing key, protocol params source
- `wallet.go` — add signing key, real address, ScriptUTxOs tracking
- `client.go` — add local-state-query for protocol params (or genesis file loading)
- `payment.go` — add signing, dynamic fees
- `delegation.go` — add signing, dynamic fees
- `governance.go` — add signing, dynamic fees
- `plutus.go` — full rebuild of both builders
- `pump.go` — pass protocol params, handle lock→unlock state machine
- `genesis.go` — load signing keys alongside UTxOs

### What this gets you
- Transactions that pass Phase-1 (valid structure, fees, signatures, collateral)
- Phase-2 script execution of always-succeeds V2 (actual plutigo evaluation in dingo)
- End-to-end Plutus validation coverage in Antithesis
- Confidence that dingo's Conway validation path works for real Plutus transactions
- All other tx types (payment, delegation, governance) also properly signed

### What this does NOT get you
- `PPViewHashesDontMatch` validation (neither dingo nor amaru enforce this yet)
- Non-trivial script testing (would need `evaluateTransaction` for real ExUnits)
- PlutusV3 coverage

---

## Reusable code from mini-gun

These functions port directly (Go → Go, same deps):

| mini-gun function | What it does | Used by |
|-------------------|-------------|---------|
| `AlwaysSucceedsScript()` | Decode hex constant | Both options |
| `AlwaysSucceedsScriptHash()` | blake2b-224(0x02 \|\| script) | Both options |
| `ScriptEnterpriseAddress()` | 0x70 \| scriptHash | Both options |
| `inlineDatumBytes()` | `[1, #6.24(cbor)]` encoding | Both options |
| `buildRedeemersCbor()` | Conway map `{[tag,idx]: [data, exunits]}` | Both options |
| `buildLangViewsCbor()` | `{0x01: cost_model_array}` | Both (A: optional) |
| `calculateScriptFee()` | `big.Rat` ceil(mem*price + cpu*price) | Option B |
| `calculateFee()` | `minFeeA * size + minFeeB` | Option B |

Note: mini-gun uses `fxamacker/cbor/v2` while txpump uses `gouroboros/cbor`.
The CBOR primitives are compatible but struct tags differ slightly.
Port would need tag translation (`cbor:",toarray"` → `cbor.StructAsArray`).

---

## Decision record

**When ready to proceed**, pick A or B based on:
- If Antithesis goal is structural fuzzing coverage → Option A
- If Antithesis goal is validation correctness testing → Option B
- Option A can be done first, then extended to B incrementally
