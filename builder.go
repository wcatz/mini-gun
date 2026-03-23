package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/blake2b"
)

var cborEnc cbor.EncMode

func init() {
	var err error
	cborEnc, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic("cbor init: " + err.Error())
	}
}

// TxInput identifies a specific UTxO to spend.
type TxInput struct {
	TxHash []byte
	Index  uint32
}

// BuiltTx holds a serialized transaction ready for submission.
type BuiltTx struct {
	CborBytes []byte
	TxHash    [32]byte
	Fee       uint64
}

// --- CBOR-encodable types matching Cardano Babbage-era wire format ---

type cborTxInput struct {
	_      struct{} `cbor:",toarray"`
	TxHash []byte
	Index  uint32
}

type cborTxOutput struct {
	_       struct{} `cbor:",toarray"`
	Address []byte
	Amount  uint64
}

type cborTxBody struct {
	Inputs      cbor.Tag       `cbor:"0,keyasint"`
	Outputs     []cborTxOutput `cbor:"1,keyasint"`
	Fee         uint64         `cbor:"2,keyasint"`
	TTL         uint64         `cbor:"3,keyasint"`
	AuxDataHash []byte         `cbor:"7,keyasint,omitempty"`
}

type cborVkeyWitness struct {
	_         struct{} `cbor:",toarray"`
	VKey      []byte
	Signature []byte
}

type cborWitnessSet struct {
	VkeyWitnesses []cborVkeyWitness `cbor:"0,keyasint"`
}

type cborTransaction struct {
	_          struct{} `cbor:",toarray"`
	Body       cbor.RawMessage
	WitnessSet cborWitnessSet
	IsValid    bool
	AuxData    interface{}
}

// BuildAndSign constructs a simple ADA payment transaction.
//
// Layout: 1 input, 2 outputs (payment + change), 1 VKey witness.
// If paddingBytes > 0, random metadata is added to increase tx size.
func BuildAndSign(
	sender *Wallet,
	receiver *Wallet,
	amount uint64,
	input TxInput,
	inputValue uint64,
	params *ProtocolParams,
	ttl uint64,
	paddingBytes int,
) (*BuiltTx, error) {
	// Generate padding once. Both passes use the same bytes so the tx size
	// is stable across passes — avoids a CBOR varint boundary causing the
	// second pass to produce a different size than the fee was calculated from.
	var padding []byte
	if paddingBytes > 0 {
		padding = make([]byte, paddingBytes)
		rand.Read(padding) //nolint:errcheck
	}

	// Two-pass fee estimation: build with estimate, then rebuild with actual fee.
	estimatedSize := uint64(400)
	if paddingBytes > 0 {
		estimatedSize += uint64(paddingBytes) + 20 // metadata overhead
	}
	fee := calculateFee(estimatedSize, params)
	tx, err := buildTx(sender, receiver, amount, input, inputValue, fee, ttl, padding)
	if err != nil {
		return nil, err
	}

	actualFee := calculateFee(uint64(len(tx.CborBytes)), params)
	if actualFee != fee {
		tx, err = buildTx(sender, receiver, amount, input, inputValue, actualFee, ttl, padding)
		if err != nil {
			return nil, err
		}
	}

	return tx, nil
}

func buildTx(
	sender *Wallet,
	receiver *Wallet,
	amount uint64,
	input TxInput,
	inputValue uint64,
	fee uint64,
	ttl uint64,
	padding []byte, // pre-generated random bytes; nil means no metadata
) (*BuiltTx, error) {
	if inputValue < amount+fee {
		return nil, fmt.Errorf("insufficient funds: have %d, need %d (amount) + %d (fee)",
			inputValue, amount, fee)
	}
	change := inputValue - amount - fee

	// Build auxiliary data (metadata) if padding requested.
	// Cardano limits individual metadata bytestrings to 64 bytes,
	// so we chunk the padding into a list of 64-byte pieces.
	var auxData interface{}
	var auxDataHash []byte
	if len(padding) > 0 {
		var chunks [][]byte
		rem := padding
		for len(rem) > 0 {
			end := 64
			if end > len(rem) {
				end = len(rem)
			}
			chunks = append(chunks, rem[:end])
			rem = rem[end:]
		}
		// Metadata map: {674: [h'<64 bytes>', h'<64 bytes>', ...]}
		metadataMap := map[uint]interface{}{674: chunks}
		auxBytes, err := cborEnc.Marshal(metadataMap)
		if err != nil {
			return nil, fmt.Errorf("encode metadata: %w", err)
		}
		auxData = cbor.RawMessage(auxBytes)
		h := blake2b.Sum256(auxBytes)
		auxDataHash = h[:]
	}

	body := cborTxBody{
		Inputs: cbor.Tag{Number: 258, Content: []cborTxInput{
			{TxHash: input.TxHash, Index: input.Index},
		}},
		Outputs: []cborTxOutput{
			{Address: receiver.Address, Amount: amount},
			{Address: sender.Address, Amount: change},
		},
		Fee:         fee,
		TTL:         ttl,
		AuxDataHash: auxDataHash,
	}

	bodyBytes, err := cborEnc.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode tx body: %w", err)
	}

	txHash := blake2b.Sum256(bodyBytes)
	sig := ed25519.Sign(sender.SigningKey, txHash[:])

	tx := cborTransaction{
		Body: cbor.RawMessage(bodyBytes),
		WitnessSet: cborWitnessSet{
			VkeyWitnesses: []cborVkeyWitness{
				{VKey: []byte(sender.VerifyKey), Signature: sig},
			},
		},
		IsValid: true,
		AuxData: auxData,
	}

	txBytes, err := cborEnc.Marshal(tx)
	if err != nil {
		return nil, fmt.Errorf("encode transaction: %w", err)
	}

	return &BuiltTx{
		CborBytes: txBytes,
		TxHash:    txHash,
		Fee:       fee,
	}, nil
}

// BuildSplitTx constructs a transaction that splits one UTXO into N equal outputs
// to the same address. Used to create contention-free UTXOs for parallel mini-gun instances.
func BuildSplitTx(
	wallet *Wallet,
	input TxInput,
	inputValue uint64,
	numOutputs int,
	params *ProtocolParams,
	ttl uint64,
) (*BuiltTx, error) {
	// Estimate: ~200 bytes base + ~60 bytes per output
	estimatedSize := uint64(200 + numOutputs*60)
	fee := calculateFee(estimatedSize, params)
	tx, err := buildSplitTx(wallet, input, inputValue, numOutputs, fee, ttl)
	if err != nil {
		return nil, err
	}

	actualFee := calculateFee(uint64(len(tx.CborBytes)), params)
	if actualFee != fee {
		tx, err = buildSplitTx(wallet, input, inputValue, numOutputs, actualFee, ttl)
		if err != nil {
			return nil, err
		}
	}

	return tx, nil
}

func buildSplitTx(
	wallet *Wallet,
	input TxInput,
	inputValue uint64,
	numOutputs int,
	fee uint64,
	ttl uint64,
) (*BuiltTx, error) {
	if inputValue <= fee {
		return nil, fmt.Errorf("insufficient funds: have %d, fee %d", inputValue, fee)
	}
	remaining := inputValue - fee
	perOutput := remaining / uint64(numOutputs)
	// Last output absorbs rounding dust
	lastOutput := remaining - perOutput*uint64(numOutputs-1)

	if perOutput < 1_000_000 {
		return nil, fmt.Errorf("per-output value %d below min-UTXO (~1 ADA)", perOutput)
	}

	outputs := make([]cborTxOutput, numOutputs)
	for i := 0; i < numOutputs-1; i++ {
		outputs[i] = cborTxOutput{Address: wallet.Address, Amount: perOutput}
	}
	outputs[numOutputs-1] = cborTxOutput{Address: wallet.Address, Amount: lastOutput}

	body := cborTxBody{
		Inputs: cbor.Tag{Number: 258, Content: []cborTxInput{
			{TxHash: input.TxHash, Index: input.Index},
		}},
		Outputs: outputs,
		Fee:     fee,
		TTL:     ttl,
	}

	bodyBytes, err := cborEnc.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode tx body: %w", err)
	}

	txHash := blake2b.Sum256(bodyBytes)
	sig := ed25519.Sign(wallet.SigningKey, txHash[:])

	tx := cborTransaction{
		Body: cbor.RawMessage(bodyBytes),
		WitnessSet: cborWitnessSet{
			VkeyWitnesses: []cborVkeyWitness{
				{VKey: []byte(wallet.VerifyKey), Signature: sig},
			},
		},
		IsValid: true,
		AuxData: nil,
	}

	txBytes, err := cborEnc.Marshal(tx)
	if err != nil {
		return nil, fmt.Errorf("encode transaction: %w", err)
	}

	return &BuiltTx{
		CborBytes: txBytes,
		TxHash:    txHash,
		Fee:       fee,
	}, nil
}

func calculateFee(txSize uint64, params *ProtocolParams) uint64 {
	return params.MinFeeCoefficient*txSize + params.MinFeeConstant.Ada.Lovelace
}

func ParseTxHash(hashHex string) ([]byte, error) {
	b, err := hex.DecodeString(hashHex)
	if err != nil {
		return nil, fmt.Errorf("decode tx hash: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("tx hash must be 32 bytes, got %d", len(b))
	}
	return b, nil
}
