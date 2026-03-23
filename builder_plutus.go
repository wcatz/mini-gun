package main

// Plutus script transaction builders.
//
// Two transaction types:
//
//   BuildScriptLockTx  — sends ADA to a script address with an inline datum.
//                        No witnesses or redeemers required.
//
//   BuildScriptUnlockTx — spends a script UTXO.
//                         Requires: script in witness set, redeemer,
//                         collateral input, collateral return, and
//                         script_integrity_hash in the tx body.
//
// Both use the always-succeeds PlutusV2 script from the antithesis txpump:
//   4e4d01000033222220051200120011
//
// The script hash (payment credential of the script address) is the
// blake2b-224 of the double-CBOR-wrapped script bytes.

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/blake2b"
)

// alwaysSucceedsV2Hex is the minimal Plutus V2 always-succeeds script in CBOR hex.
// It is the double-CBOR-wrapped inner bytes as stored in the witness set.
const alwaysSucceedsV2Hex = "4e4d01000033222220051200120011"

// AlwaysSucceedsScript returns the raw bytes of the always-succeeds V2 script.
func AlwaysSucceedsScript() []byte {
	b, _ := hex.DecodeString(alwaysSucceedsV2Hex)
	return b
}

// AlwaysSucceedsScriptHash returns the 28-byte blake2b-224 hash of the
// double-CBOR-wrapped script — this is the payment credential used to
// derive the script address and to identify the script in the witness set.
func AlwaysSucceedsScriptHash() []byte {
	// The script bytes are already the inner (unwrapped) bytes.
	// Per Cardano ledger: script hash = blake2b-224(tag || script_bytes)
	// where tag = 0x02 for PlutusV2.
	raw := AlwaysSucceedsScript()
	tagged := make([]byte, 1+len(raw))
	tagged[0] = 0x02 // PlutusV2 tag
	copy(tagged[1:], raw)
	h, _ := blake2b.New(28, nil)
	h.Write(tagged)
	return h.Sum(nil)
}

// ScriptEnterpriseAddress builds a 29-byte Conway enterprise address for a
// script payment credential (no staking credential).
//
// Header byte = (AddressTypeScriptNone << 4) | network_id
//   AddressTypeScriptNone = 0b0111 = 7
//   mainnet network_id    = 1  → header 0x71
//   testnet network_id    = 0  → header 0x70  (preview uses testnet)
func ScriptEnterpriseAddress(scriptHash []byte, mainnet bool) []byte {
	addr := make([]byte, 29)
	if mainnet {
		addr[0] = 0x71 // (7 << 4) | 1 — mainnet
	} else {
		addr[0] = 0x70 // (7 << 4) | 0 — testnet (preview)
	}
	copy(addr[1:], scriptHash)
	return addr
}

// --- CBOR structures for Conway Plutus transactions ---

// cborConwayTxBody is a Conway tx body. We encode it as a CBOR map
// manually so we can include only the keys we actually need.
// Fields match ConwayTransactionBody key numbers.
type cborConwayTxBody struct {
	// key 0: inputs (set, tag 258)
	// key 1: outputs (array of map-encoded Babbage outputs)
	// key 2: fee
	// key 3: ttl
	// key 11: script_data_hash
	// key 13: collateral inputs (set, tag 258) — unlock only
	// key 16: collateral return — unlock only
	// key 17: total_collateral — unlock only
}

// cborBabbageOutput is a map-encoded output (Babbage/Conway post-Alonzo format).
// key 0 = address, key 1 = value (lovelace), key 2 = datum_option (optional).
type cborBabbageOutput struct {
	Address     []byte        `cbor:"0,keyasint"`
	Amount      uint64        `cbor:"1,keyasint"`
	DatumOption *cborDatumOpt `cbor:"2,keyasint,omitempty"`
}

// cborDatumOpt encodes [1, #6.24(bstr)] for an inline datum.
// We store it as raw CBOR so we can control the tag.
type cborDatumOpt struct {
	raw []byte
}

func (d cborDatumOpt) MarshalCBOR() ([]byte, error) {
	return d.raw, nil
}

// inlineDatumBytes builds the CBOR encoding of a Babbage inline datum option:
// [1, #6.24(bstr)] where bstr is the CBOR encoding of the datum value.
func inlineDatumBytes(datumCbor []byte) ([]byte, error) {
	// #6.24(bstr) = tag 24 wrapping the datum bytes as a bytestring
	wrapped, err := cborEnc.Marshal(cbor.Tag{Number: 24, Content: datumCbor})
	if err != nil {
		return nil, err
	}
	return cborEnc.Marshal([]interface{}{uint64(1), cbor.RawMessage(wrapped)})
}

// cborConwayWitnessSet is the witness set for a Conway script transaction.
// key 0 = vkey witnesses, key 5 = redeemers (map), key 6 = plutus_v2_scripts.
type cborConwayWitnessSet struct {
	VkeyWitnesses   []cborVkeyWitness       `cbor:"0,keyasint"`
	Redeemers       *cborConwayRedeemerMap  `cbor:"5,keyasint,omitempty"`
	PlutusV2Scripts [][]byte                `cbor:"6,keyasint,omitempty"`
}

// cborConwayRedeemerMap encodes as a CBOR map {[tag,index]: [data, [mem,steps]]}.
// We keep it as raw CBOR so we can precisely control the bytes for hashing.
type cborConwayRedeemerMap struct {
	raw []byte
}

func (r *cborConwayRedeemerMap) MarshalCBOR() ([]byte, error) {
	return r.raw, nil
}

// cborConwayTx is the top-level Conway transaction array: [body, witnesses, is_valid, aux_data].
type cborConwayTx struct {
	_          struct{} `cbor:",toarray"`
	Body       cbor.RawMessage
	WitnessSet cborConwayWitnessSet
	IsValid    bool
	AuxData    interface{}
}

// --- Lock transaction ---

// PlutusLockParams holds the parameters for a script lock transaction.
type PlutusLockParams struct {
	Sender      *Wallet
	Input       TxInput
	InputValue  uint64
	LockAmount  uint64 // ADA to lock at the script address
	Params      *ProtocolParams
	TTL         uint64
}

// BuildScriptLockTx builds a Conway transaction that sends ADA to the
// always-succeeds V2 script address with an inline unit datum.
// Layout: 1 input, 2 outputs (script output + change), 1 VKey witness.
func BuildScriptLockTx(p PlutusLockParams) (*BuiltTx, error) {
	estimatedSize := uint64(350)
	fee := calculateFee(estimatedSize, p.Params)
	tx, err := buildScriptLockTx(p, fee)
	if err != nil {
		return nil, err
	}
	actualFee := calculateFee(uint64(len(tx.CborBytes)), p.Params)
	if actualFee != fee {
		tx, err = buildScriptLockTx(p, actualFee)
		if err != nil {
			return nil, err
		}
	}
	return tx, nil
}

func buildScriptLockTx(p PlutusLockParams, fee uint64) (*BuiltTx, error) {
	if p.InputValue < p.LockAmount+fee {
		return nil, fmt.Errorf("insufficient funds: have %d, need %d + %d fee",
			p.InputValue, p.LockAmount, fee)
	}
	change := p.InputValue - p.LockAmount - fee

	scriptHash := AlwaysSucceedsScriptHash()
	scriptAddr := ScriptEnterpriseAddress(scriptHash, false) // preview = testnet

	// Inline datum: unit = CBOR #6.121([]) = Constr 0 []
	// In Plutus data: Constr 0 [] encodes as d87980 (alternative 0, no fields).
	unitDatum := []byte{0xd8, 0x79, 0x80}

	datumOptBytes, err := inlineDatumBytes(unitDatum)
	if err != nil {
		return nil, fmt.Errorf("encode inline datum: %w", err)
	}

	// Build outputs as raw CBOR using map encoding (Babbage format).
	scriptOut := cborBabbageOutput{
		Address:     scriptAddr,
		Amount:      p.LockAmount,
		DatumOption: &cborDatumOpt{raw: datumOptBytes},
	}
	scriptOutBytes, err := cborEnc.Marshal(scriptOut)
	if err != nil {
		return nil, fmt.Errorf("encode script output: %w", err)
	}

	changeOut := cborBabbageOutput{
		Address: p.Sender.Address,
		Amount:  change,
	}
	changeOutBytes, err := cborEnc.Marshal(changeOut)
	if err != nil {
		return nil, fmt.Errorf("encode change output: %w", err)
	}

	// Build tx body as a raw CBOR map so we control the exact key set.
	bodyMap := map[uint64]interface{}{
		0: cbor.Tag{Number: 258, Content: []cborTxInput{
			{TxHash: p.Input.TxHash, Index: p.Input.Index},
		}},
		1: []cbor.RawMessage{scriptOutBytes, changeOutBytes},
		2: fee,
		3: p.TTL,
	}
	bodyBytes, err := cborEnc.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("encode tx body: %w", err)
	}

	txHash := blake2b.Sum256(bodyBytes)
	sig := ed25519.Sign(p.Sender.SigningKey, txHash[:])

	tx := cborConwayTx{
		Body: cbor.RawMessage(bodyBytes),
		WitnessSet: cborConwayWitnessSet{
			VkeyWitnesses: []cborVkeyWitness{
				{VKey: []byte(p.Sender.VerifyKey), Signature: sig},
			},
		},
		IsValid: true,
		AuxData: nil,
	}

	txBytes, err := cborEnc.Marshal(tx)
	if err != nil {
		return nil, fmt.Errorf("encode tx: %w", err)
	}

	return &BuiltTx{CborBytes: txBytes, TxHash: txHash, Fee: fee}, nil
}

// --- Unlock transaction ---

// PlutusUnlockParams holds the parameters for a script unlock transaction.
type PlutusUnlockParams struct {
	Spender         *Wallet
	ScriptInput     TxInput  // the locked script UTXO
	ScriptValue     uint64   // value at the script address
	CollateralInput TxInput  // a vkey-locked UTXO for collateral
	CollateralValue uint64   // value of the collateral UTXO
	Params          *ProtocolParams
	TTL             uint64
}

// BuildScriptUnlockTx builds a Conway transaction that spends the
// always-succeeds V2 script UTXO. Includes:
//   - Script in witness set
//   - Spend redeemer (unit data, ExUnits from Ogmios evaluate or hardcoded)
//   - Collateral input + return
//   - script_integrity_hash
func BuildScriptUnlockTx(p PlutusUnlockParams) (*BuiltTx, error) {
	// Always-succeeds uses negligible ExUnits. We use modest declared values
	// that fit within protocol limits. The node will evaluate the actual cost;
	// we just need to declare enough and pay the script fee.
	const exMem   int64 = 14
	const exSteps int64 = 10_000_000

	estimatedSize := uint64(600) // unlock txs are larger
	scriptFee := calculateScriptFee(exMem, exSteps, p.Params)
	fee := calculateFee(estimatedSize, p.Params) + scriptFee

	tx, err := buildScriptUnlockTx(p, fee, exMem, exSteps)
	if err != nil {
		return nil, err
	}

	actualFee := calculateFee(uint64(len(tx.CborBytes)), p.Params) + scriptFee
	if actualFee != fee {
		tx, err = buildScriptUnlockTx(p, actualFee, exMem, exSteps)
		if err != nil {
			return nil, err
		}
	}

	return tx, nil
}

func buildScriptUnlockTx(
	p PlutusUnlockParams,
	fee uint64,
	exMem int64,
	exSteps int64,
) (*BuiltTx, error) {
	if p.ScriptValue < fee {
		return nil, fmt.Errorf("script value %d < fee %d", p.ScriptValue, fee)
	}
	change := p.ScriptValue - fee

	// Collateral return: collateral input minus 150% of fee (collateralPercentage).
	collateralPct := p.Params.CollateralPercentage
	if collateralPct == 0 {
		collateralPct = 150 // preview default
	}
	totalCollateral := (fee*collateralPct + 99) / 100 // ceil
	if p.CollateralValue < totalCollateral {
		return nil, fmt.Errorf(
			"collateral value %d < required %d (fee=%d pct=%d)",
			p.CollateralValue, totalCollateral, fee, collateralPct,
		)
	}
	collateralReturn := p.CollateralValue - totalCollateral

	// Build the redeemer CBOR: {[0,0]: [unit, [mem, steps]]}
	// unit datum = d87980 (Constr 0 [])
	// This must be raw CBOR so we can hash it exactly for script_integrity_hash.
	redeemersCbor, err := buildRedeemersCbor(exMem, exSteps)
	if err != nil {
		return nil, fmt.Errorf("encode redeemers: %w", err)
	}

	// script_integrity_hash = blake2b-256(redeemers || datums || langviews)
	// No witness datums (inline datum on the input UTxO).
	// datumsCbor = "" (empty, no witness datums)
	langViewsCbor, err := buildLangViewsCbor(p.Params)
	if err != nil {
		return nil, fmt.Errorf("encode lang views: %w", err)
	}

	scriptIntegrityInput := make([]byte, 0, len(redeemersCbor)+len(langViewsCbor))
	scriptIntegrityInput = append(scriptIntegrityInput, redeemersCbor...)
	// datumsCbor is empty — omit entirely (not even an empty array)
	scriptIntegrityInput = append(scriptIntegrityInput, langViewsCbor...)
	scriptIntegrityHash := blake2b.Sum256(scriptIntegrityInput)

	// Output: change back to the spender.
	changeOut := cborBabbageOutput{
		Address: p.Spender.Address,
		Amount:  change,
	}
	changeOutBytes, err := cborEnc.Marshal(changeOut)
	if err != nil {
		return nil, fmt.Errorf("encode change output: %w", err)
	}

	// Collateral return output.
	collReturnOut := cborBabbageOutput{
		Address: p.Spender.Address,
		Amount:  collateralReturn,
	}
	collReturnBytes, err := cborEnc.Marshal(collReturnOut)
	if err != nil {
		return nil, fmt.Errorf("encode collateral return: %w", err)
	}

	// Build tx body as raw CBOR map.
	bodyMap := map[uint64]interface{}{
		0:  cbor.Tag{Number: 258, Content: []cborTxInput{{TxHash: p.ScriptInput.TxHash, Index: p.ScriptInput.Index}}},
		1:  []cbor.RawMessage{changeOutBytes},
		2:  fee,
		3:  p.TTL,
		11: scriptIntegrityHash[:],
		13: cbor.Tag{Number: 258, Content: []cborTxInput{{TxHash: p.CollateralInput.TxHash, Index: p.CollateralInput.Index}}},
		16: cbor.RawMessage(collReturnBytes),
		17: totalCollateral,
	}
	bodyBytes, err := cborEnc.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("encode tx body: %w", err)
	}

	txHash := blake2b.Sum256(bodyBytes)
	sig := ed25519.Sign(p.Spender.SigningKey, txHash[:])

	tx := cborConwayTx{
		Body: cbor.RawMessage(bodyBytes),
		WitnessSet: cborConwayWitnessSet{
			VkeyWitnesses: []cborVkeyWitness{
				{VKey: []byte(p.Spender.VerifyKey), Signature: sig},
			},
			Redeemers:       &cborConwayRedeemerMap{raw: redeemersCbor},
			PlutusV2Scripts: [][]byte{AlwaysSucceedsScript()},
		},
		IsValid: true,
		AuxData: nil,
	}

	txBytes, err := cborEnc.Marshal(tx)
	if err != nil {
		return nil, fmt.Errorf("encode tx: %w", err)
	}

	return &BuiltTx{CborBytes: txBytes, TxHash: txHash, Fee: fee}, nil
}

// buildRedeemersCbor encodes the Conway redeemer map:
// { [0, 0] : [d87980, [mem, steps]] }
// tag 0 = spend, index 0 = first input.
// d87980 = Constr 0 [] = unit.
// Returns the exact bytes used for script_integrity_hash.
func buildRedeemersCbor(exMem, exSteps int64) ([]byte, error) {
	// Encode as a CBOR map with one entry.
	// Key: [tag=0, index=0] (array of 2 uints)
	// Value: [unit_data, [mem, steps]] (array)
	//   unit_data = #6.121([]) = d87980
	//   exunits = [mem, steps]
	unitDatum := cbor.RawMessage{0xd8, 0x79, 0x80}

	type redeemerKey struct {
		_ struct{} `cbor:",toarray"`
		Tag   uint64
		Index uint64
	}
	type exUnits struct {
		_ struct{} `cbor:",toarray"`
		Mem   int64
		Steps int64
	}
	type redeemerValue struct {
		_ struct{} `cbor:",toarray"`
		Data    cbor.RawMessage
		ExUnits exUnits
	}

	key := redeemerKey{Tag: 0, Index: 0}
	val := redeemerValue{
		Data:    unitDatum,
		ExUnits: exUnits{Mem: exMem, Steps: exSteps},
	}

	// Conway redeemers are encoded as a CBOR map.
	// fxamacker encodes map[interface{}]interface{} with canonical key ordering.
	// We need to use a structured type to get the right CBOR.
	keyBytes, err := cborEnc.Marshal(key)
	if err != nil {
		return nil, err
	}
	valBytes, err := cborEnc.Marshal(val)
	if err != nil {
		return nil, err
	}

	// Build raw CBOR map with 1 entry: 0xa1 <key> <value>
	result := make([]byte, 0, 1+len(keyBytes)+len(valBytes))
	result = append(result, 0xa1) // map(1)
	result = append(result, keyBytes...)
	result = append(result, valBytes...)
	return result, nil
}

// buildLangViewsCbor encodes the language views for PlutusV2 (version index 1).
// Format per Cardano spec:
//   { 1: cost_model_params }  (definite-length map, key = raw byte 0x01)
// Cost model values are from the protocol params CostModels["plutus:v2"].
func buildLangViewsCbor(params *ProtocolParams) ([]byte, error) {
	costModel, ok := params.CostModels["plutus:v2"]
	if !ok {
		return nil, fmt.Errorf("no PlutusV2 cost model in protocol params")
	}

	// Per EncodeLangViews for version 1 (PlutusV2):
	// tag = single byte 0x01
	// params = definite-length CBOR array of int64 values
	paramBytes, err := cborEnc.Marshal(costModel)
	if err != nil {
		return nil, fmt.Errorf("encode cost model: %w", err)
	}

	// Result: 0xa1 <tag_byte=0x01> <params>
	result := make([]byte, 0, 1+1+len(paramBytes))
	result = append(result, 0xa1) // map(1)
	result = append(result, 0x01) // key: integer 1 (PlutusV2)
	result = append(result, paramBytes...)
	return result, nil
}

// calculateScriptFee computes the script execution fee component:
// ceil(pricesMem * exMem + pricesSteps * exSteps)
// using exact rational arithmetic.
func calculateScriptFee(exMem, exSteps int64, params *ProtocolParams) uint64 {
	memNum := params.ScriptExecutionPrices.Memory.Numerator
	memDen := params.ScriptExecutionPrices.Memory.Denominator
	cpuNum := params.ScriptExecutionPrices.CPU.Numerator
	cpuDen := params.ScriptExecutionPrices.CPU.Denominator

	if memDen == 0 || cpuDen == 0 {
		return 0
	}

	// sum = (memNum/memDen)*exMem + (cpuNum/cpuDen)*exSteps
	memCost := new(big.Rat).Mul(
		new(big.Rat).SetFrac(big.NewInt(memNum), big.NewInt(memDen)),
		new(big.Rat).SetInt64(exMem),
	)
	cpuCost := new(big.Rat).Mul(
		new(big.Rat).SetFrac(big.NewInt(cpuNum), big.NewInt(cpuDen)),
		new(big.Rat).SetInt64(exSteps),
	)
	sum := new(big.Rat).Add(memCost, cpuCost)

	// ceil(sum)
	num := sum.Num()
	denom := sum.Denom()
	q, r := new(big.Int).DivMod(num, denom, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsUint64() {
		return ^uint64(0)
	}
	return q.Uint64()
}

// sortInputsForRedeemerIndex returns the canonical (lexicographic by txhash+index)
// sorted position of a given input. The redeemer index must match the position
// of the input in the canonical sorted list of transaction inputs.
func sortedInputIndex(inputs []TxInput, target TxInput) (uint32, error) {
	sorted := make([]TxInput, len(inputs))
	copy(sorted, inputs)
	sort.Slice(sorted, func(i, j int) bool {
		cmp := compareBytes(sorted[i].TxHash, sorted[j].TxHash)
		if cmp != 0 {
			return cmp < 0
		}
		return sorted[i].Index < sorted[j].Index
	})
	for i, inp := range sorted {
		if compareBytes(inp.TxHash, target.TxHash) == 0 && inp.Index == target.Index {
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf("input not found in sorted list")
}

func compareBytes(a, b []byte) int {
	la, lb := len(a), len(b)
	min := la
	if lb < min {
		min = lb
	}
	for i := 0; i < min; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	if la < lb {
		return -1
	}
	if la > lb {
		return 1
	}
	return 0
}


