package main

import (
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/blake2b"
)

func TestVrfNonceValue(t *testing.T) {
	// vrfNonceValue = BLAKE2b-256(0x4E || vrfOutput)
	vrfOutput := make([]byte, 64)
	for i := range vrfOutput {
		vrfOutput[i] = byte(i)
	}

	result := vrfNonceValue(vrfOutput)

	// Verify manually
	h, _ := blake2b.New256(nil)
	h.Write([]byte{0x4E})
	h.Write(vrfOutput)
	expected := h.Sum(nil)

	if hex.EncodeToString(result) != hex.EncodeToString(expected) {
		t.Fatalf("vrfNonceValue mismatch:\n  got:  %s\n  want: %s",
			hex.EncodeToString(result), hex.EncodeToString(expected))
	}

	if len(result) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(result))
	}
}

func TestEvolveNonce(t *testing.T) {
	// evolveNonce = BLAKE2b-256(currentNonce || nonceValue)
	currentNonce := make([]byte, 32)
	currentNonce[0] = 0xAA
	nonceValue := make([]byte, 32)
	nonceValue[0] = 0xBB

	result := evolveNonce(currentNonce, nonceValue)

	// Verify manually
	h, _ := blake2b.New256(nil)
	h.Write(currentNonce)
	h.Write(nonceValue)
	expected := h.Sum(nil)

	if hex.EncodeToString(result) != hex.EncodeToString(expected) {
		t.Fatalf("evolveNonce mismatch:\n  got:  %s\n  want: %s",
			hex.EncodeToString(result), hex.EncodeToString(expected))
	}
}

func TestInitialNonceFullMode(t *testing.T) {
	nonce := initialNonce(true)
	expected, _ := hex.DecodeString(ShelleyGenesisHash)

	if hex.EncodeToString(nonce) != hex.EncodeToString(expected) {
		t.Fatalf("full mode nonce mismatch:\n  got:  %s\n  want: %s",
			hex.EncodeToString(nonce), hex.EncodeToString(expected))
	}
}

func TestInitialNonceLiteMode(t *testing.T) {
	nonce := initialNonce(false)

	if len(nonce) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(nonce))
	}
	for i, b := range nonce {
		if b != 0 {
			t.Fatalf("lite mode nonce byte %d should be 0, got 0x%x", i, b)
		}
	}
}
