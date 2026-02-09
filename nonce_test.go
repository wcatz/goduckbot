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
	// evolveNonce = XOR (Cardano Nonce semigroup)
	currentNonce := make([]byte, 32)
	currentNonce[0] = 0xAA
	nonceValue := make([]byte, 32)
	nonceValue[0] = 0xBB

	result := evolveNonce(currentNonce, nonceValue)

	// XOR: 0xAA ^ 0xBB = 0x11, rest are 0x00 ^ 0x00 = 0x00
	if result[0] != 0x11 {
		t.Fatalf("evolveNonce byte[0] mismatch: got 0x%x, want 0x11", result[0])
	}
	for i := 1; i < 32; i++ {
		if result[i] != 0 {
			t.Fatalf("evolveNonce byte[%d] mismatch: got 0x%x, want 0x00", i, result[i])
		}
	}
}

func TestXorBytes(t *testing.T) {
	a := make([]byte, 32)
	b := make([]byte, 32)
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(0xFF - i)
	}
	result := xorBytes(a, b)
	for i := range result {
		expected := byte(i) ^ byte(0xFF-i)
		if result[i] != expected {
			t.Fatalf("xorBytes byte[%d]: got 0x%x, want 0x%x", i, result[i], expected)
		}
	}

	// XOR with zeros is identity
	zeros := make([]byte, 32)
	result = xorBytes(a, zeros)
	if hex.EncodeToString(result) != hex.EncodeToString(a) {
		t.Fatalf("xorBytes with zeros should be identity")
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
