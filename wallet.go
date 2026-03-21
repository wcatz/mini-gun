package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// Wallet holds a Cardano payment key pair and address.
type Wallet struct {
	SigningKey ed25519.PrivateKey
	VerifyKey  ed25519.PublicKey
	Address    []byte // raw address bytes (decoded from bech32)
	AddrBech32 string
}

type keyEnvelope struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	CborHex     string `json:"cborHex"`
}

// LoadWallet reads a Cardano signing key JSON file and parses a bech32 address.
func LoadWallet(skeyPath, addrBech32 string) (*Wallet, error) {
	data, err := os.ReadFile(skeyPath)
	if err != nil {
		return nil, fmt.Errorf("read skey: %w", err)
	}

	var env keyEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse skey json: %w", err)
	}

	cborBytes, err := hex.DecodeString(env.CborHex)
	if err != nil {
		return nil, fmt.Errorf("decode cborHex: %w", err)
	}

	var seed []byte
	if err := cbor.Unmarshal(cborBytes, &seed); err != nil {
		return nil, fmt.Errorf("cbor decode skey: %w", err)
	}

	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("unexpected key size: %d (expected %d)", len(seed), ed25519.SeedSize)
	}

	sk := ed25519.NewKeyFromSeed(seed)
	vk := sk.Public().(ed25519.PublicKey)

	addrBytes, err := decodeBech32Address(addrBech32)
	if err != nil {
		return nil, fmt.Errorf("decode address: %w", err)
	}

	return &Wallet{
		SigningKey: sk,
		VerifyKey:  vk,
		Address:    addrBytes,
		AddrBech32: addrBech32,
	}, nil
}

// --- bech32 decoding (Cardano uses standard bech32) ---

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func decodeBech32Address(bech string) ([]byte, error) {
	bech = strings.ToLower(bech)
	pos := strings.LastIndex(bech, "1")
	if pos < 1 || pos+7 > len(bech) {
		return nil, fmt.Errorf("invalid bech32: no separator")
	}

	hrp := bech[:pos]
	dataPart := bech[pos+1:]

	values := make([]byte, len(dataPart))
	for i, c := range dataPart {
		idx := strings.IndexByte(bech32Charset, byte(c))
		if idx < 0 {
			return nil, fmt.Errorf("invalid bech32 character: %c", c)
		}
		values[i] = byte(idx)
	}

	if !bech32VerifyChecksum(hrp, values) {
		return nil, fmt.Errorf("invalid bech32 checksum")
	}

	// Strip 6-byte checksum
	values = values[:len(values)-6]

	return convertBits(values, 5, 8, false)
}

func bech32VerifyChecksum(hrp string, values []byte) bool {
	all := append(bech32HrpExpand(hrp), values...)
	return bech32Polymod(all) == 1
}

func bech32HrpExpand(hrp string) []byte {
	ret := make([]byte, len(hrp)*2+1)
	for i, c := range hrp {
		ret[i] = byte(c >> 5)
		ret[i+len(hrp)+1] = byte(c & 31)
	}
	return ret
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func convertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	acc := uint32(0)
	bits := uint(0)
	var ret []byte
	maxv := uint32((1 << toBits) - 1)

	for _, value := range data {
		acc = (acc << fromBits) | uint32(value)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			ret = append(ret, byte((acc>>bits)&maxv))
		}
	}

	if pad {
		if bits > 0 {
			ret = append(ret, byte((acc<<(toBits-bits))&maxv))
		}
	} else if bits >= fromBits {
		return nil, fmt.Errorf("invalid padding")
	} else if (acc<<(toBits-bits))&maxv != 0 {
		return nil, fmt.Errorf("non-zero padding")
	}

	return ret, nil
}
