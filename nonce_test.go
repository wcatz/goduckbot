package main

import (
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/blake2b"
)

func TestVrfNonceValue(t *testing.T) {
	// vrfNonceValue = BLAKE2b-256(vrfOutput) — bare hash, no prefix
	vrfOutput := make([]byte, 64)
	for i := range vrfOutput {
		vrfOutput[i] = byte(i)
	}

	result := vrfNonceValue(vrfOutput)

	// Verify manually
	h, _ := blake2b.New256(nil)
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

// TestPallasRollingNonce verifies against pallas/cncli test vectors from
// pallas-crypto/src/nonce/mod.rs
func TestPallasRollingNonce(t *testing.T) {
	genesisHash, _ := hex.DecodeString("1a3be38bcbb7911969283716ad7aa550250226b76a61fc51cc9a9a35d9276d81")

	// First 5 VRF outputs from pallas test_rolling_nonce
	vrfOutputs := []string{
		"36ec5378d1f5041a59eb8d96e61de96f0950fb41b49ff511f7bc7fd109d4383e1d24be7034e6749c6612700dd5ceb0c66577b88a19ae286b1321d15bce1ab736",
		"e0bf34a6b73481302f22987cde4c12807cbc2c3fea3f7fcb77261385a50e8ccdda3226db3efff73e9fb15eecf841bbc85ce37550de0435ebcdcb205e0ed08467",
		"7107ef8c16058b09f4489715297e55d145a45fc0df75dfb419cab079cd28992854a034ad9dc4c764544fb70badd30a9611a942a03523c6f3d8967cf680c4ca6b",
		"6f561aad83884ee0d7b19fd3d757c6af096bfd085465d1290b13a9dfc817dfcdfb0b59ca06300206c64d1ba75fd222a88ea03c54fbbd5d320b4fbcf1c228ba4e",
		"3d3ba80724db0a028783afa56a85d684ee778ae45b9aa9af3120f5e1847be1983bd4868caf97fcfd82d5a3b0b7c1a6d53491d75440a75198014eb4e707785cad",
	}
	expectedEtaVs := []string{
		"2af15f57076a8ff225746624882a77c8d2736fe41d3db70154a22b50af851246",
		"a815ff978369b57df09b0072485c26920dc0ec8e924a852a42f0715981cf0042",
		"f112d91435b911b6b5acaf27198762905b1cdec8c5a7b712f925ce3c5c76bb5f",
		"5450d95d9be4194a0ded40fbb4036b48d1f1d6da796e933fefd2c5c888794b4b",
		"c5c0f406cb522ad3fead4ecc60bce9c31e80879bc17eb1bb9acaa9b998cdf8bf",
	}

	etaV := make([]byte, 32)
	copy(etaV, genesisHash)

	for i, vrfHex := range vrfOutputs {
		vrfOutput, _ := hex.DecodeString(vrfHex)
		nonceValue := vrfNonceValue(vrfOutput)
		etaV = evolveNonce(etaV, nonceValue)

		got := hex.EncodeToString(etaV)
		if got != expectedEtaVs[i] {
			t.Fatalf("rolling nonce mismatch at block %d:\n  got:  %s\n  want: %s", i, got, expectedEtaVs[i])
		}
	}
}

// TestPallasEpochNonce verifies epoch nonce against pallas test vectors
func TestPallasEpochNonce(t *testing.T) {
	nc, _ := hex.DecodeString("e86e133bd48ff5e79bec43af1ac3e348b539172f33e502d2c96735e8c51bd04d")
	nh, _ := hex.DecodeString("d7a1ff2a365abed59c9ae346cba842b6d3df06d055dba79a113e0704b44cc3e9")
	expected := "e536a0081ddd6d19786e9d708a85819a5c3492c0da7349f59c8ad3e17e4acd98"

	result := hashConcat(nc, nh)
	got := hex.EncodeToString(result)
	if got != expected {
		t.Fatalf("epoch nonce mismatch:\n  got:  %s\n  want: %s", got, expected)
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

func TestVrfNonceValueForEpochShelley(t *testing.T) {
	// Shelley era (epoch 208) — no domain separator, same as vrfNonceValue
	vrfOutput, _ := hex.DecodeString("36ec5378d1f5041a59eb8d96e61de96f0950fb41b49ff511f7bc7fd109d4383e1d24be7034e6749c6612700dd5ceb0c66577b88a19ae286b1321d15bce1ab736")
	got := vrfNonceValueForEpoch(vrfOutput, 208, MainnetNetworkMagic)
	want := vrfNonceValue(vrfOutput)
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("Shelley era should match vrfNonceValue:\n  got:  %s\n  want: %s",
			hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

func TestVrfNonceValueForEpochBabbage(t *testing.T) {
	// Babbage era (epoch 365+) — needs 0x4E domain separator + double hash
	vrfOutput := make([]byte, 64)
	for i := range vrfOutput {
		vrfOutput[i] = byte(i)
	}
	got := vrfNonceValueForEpoch(vrfOutput, 365, MainnetNetworkMagic)

	// Expected: BLAKE2b-256(BLAKE2b-256(0x4E || vrfOutput))
	h1, _ := blake2b.New256(nil)
	h1.Write([]byte{0x4E})
	h1.Write(vrfOutput)
	tagged := h1.Sum(nil)
	h2, _ := blake2b.New256(nil)
	h2.Write(tagged)
	want := h2.Sum(nil)

	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("Babbage era domain separator mismatch:\n  got:  %s\n  want: %s",
			hex.EncodeToString(got), hex.EncodeToString(want))
	}

	// Must differ from non-domain-separated
	plain := vrfNonceValue(vrfOutput)
	if hex.EncodeToString(got) == hex.EncodeToString(plain) {
		t.Fatal("Babbage nonce should differ from Shelley (domain separator missing)")
	}
}

func TestStabilityWindowTPraos(t *testing.T) {
	// Epoch 208 (Shelley/TPraos): 3k/f = 129600 margin → freeze at 302400
	got := StabilityWindowSlotsForEpoch(208, MainnetNetworkMagic)
	if got != 302400 {
		t.Fatalf("TPraos stability window: got %d, want 302400", got)
	}
}

func TestStabilityWindowBabbage(t *testing.T) {
	// Epoch 365 (Babbage): still 3k/f = 129600 margin → freeze at 302400
	// Babbage uses computeStabilityWindow (3k/f), NOT computeRandomnessStabilisationWindow (4k/f)
	got := StabilityWindowSlotsForEpoch(365, MainnetNetworkMagic)
	if got != 302400 {
		t.Fatalf("Babbage stability window: got %d, want 302400", got)
	}
}

func TestStabilityWindowConway(t *testing.T) {
	// Epoch 507 (Conway): 4k/f = 172800 margin → freeze at 259200
	got := StabilityWindowSlotsForEpoch(507, MainnetNetworkMagic)
	if got != 259200 {
		t.Fatalf("Conway stability window: got %d, want 259200", got)
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
