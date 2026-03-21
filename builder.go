package main

import (
	"crypto/ed25519"
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
	Inputs  cbor.Tag       `cbor:"0,keyasint"`
	Outputs []cborTxOutput `cbor:"1,keyasint"`
	Fee     uint64         `cbor:"2,keyasint"`
	TTL     uint64         `cbor:"3,keyasint"`
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
func BuildAndSign(
	sender *Wallet,
	receiver *Wallet,
	amount uint64,
	input TxInput,
	inputValue uint64,
	params *ProtocolParams,
	ttl uint64,
) (*BuiltTx, error) {
	// Two-pass fee estimation: build with estimate, then rebuild with actual fee.
	fee := estimateFee(params)
	tx, err := buildTx(sender, receiver, amount, input, inputValue, fee, ttl)
	if err != nil {
		return nil, err
	}

	actualFee := calculateFee(uint64(len(tx.CborBytes)), params)
	if actualFee != fee {
		tx, err = buildTx(sender, receiver, amount, input, inputValue, actualFee, ttl)
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
) (*BuiltTx, error) {
	if inputValue < amount+fee {
		return nil, fmt.Errorf("insufficient funds: have %d, need %d (amount) + %d (fee)",
			inputValue, amount, fee)
	}
	change := inputValue - amount - fee

	body := cborTxBody{
		Inputs: cbor.Tag{Number: 258, Content: []cborTxInput{
			{TxHash: input.TxHash, Index: input.Index},
		}},
		Outputs: []cborTxOutput{
			{Address: receiver.Address, Amount: amount},
			{Address: sender.Address, Amount: change},
		},
		Fee: fee,
		TTL: ttl,
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

func estimateFee(params *ProtocolParams) uint64 {
	// A simple 1-in 2-out tx is ~300 bytes. Overestimate to avoid rebuild.
	return calculateFee(400, params)
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
