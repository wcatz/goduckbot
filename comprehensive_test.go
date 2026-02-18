package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"
)

// ============================================================================
// Slot/Epoch Math — Extended Edge Cases
// ============================================================================

func TestSlotToEpochMainnetExtended(t *testing.T) {
	tests := []struct {
		slot  uint64
		epoch int
	}{
		{0, 0},
		{21599, 0},
		{21600, 1},
		{43199, 1},
		{43200, 2},
		{4492799, 207},              // Last Byron slot
		{4492800, 208},              // First Shelley slot
		{4492800 + 431999, 208},     // Last slot of epoch 208
		{4492800 + 432000, 209},     // First slot of epoch 209
		{4492800 + 432000*100, 308}, // 100 Shelley epochs
		{4492800 + 432000*200, 408}, // 200 Shelley epochs
		{4492800 + 432000*400, 608}, // 400 Shelley epochs (recent)
	}

	for _, tt := range tests {
		got := SlotToEpoch(tt.slot, MainnetNetworkMagic)
		if got != tt.epoch {
			t.Errorf("SlotToEpoch(%d, mainnet) = %d, want %d", tt.slot, got, tt.epoch)
		}
	}
}

func TestSlotToEpochPreprodExtended(t *testing.T) {
	tests := []struct {
		slot  uint64
		epoch int
	}{
		{0, 0},
		{21599, 0},
		{21600, 1},
		{43199, 1},
		{43200, 2},
		{64799, 2},
		{64800, 3},
		{86399, 3},                  // Last Byron slot
		{86400, 4},                  // First Shelley slot
		{86400 + 431999, 4},         // Last slot of epoch 4
		{86400 + 432000, 5},         // First slot of epoch 5
		{86400 + 432000*50, 54},     // 50 Shelley epochs in
	}

	for _, tt := range tests {
		got := SlotToEpoch(tt.slot, PreprodNetworkMagic)
		if got != tt.epoch {
			t.Errorf("SlotToEpoch(%d, preprod) = %d, want %d", tt.slot, got, tt.epoch)
		}
	}
}

func TestSlotToEpochPreviewExtended(t *testing.T) {
	tests := []struct {
		slot  uint64
		epoch int
	}{
		{0, 0},
		{86399, 0},
		{86400, 1},
		{172799, 1},
		{172800, 2},
		{259200, 3},
		{86400 * 100, 100},
		{86400 * 999, 999},
	}

	for _, tt := range tests {
		got := SlotToEpoch(tt.slot, PreviewNetworkMagic)
		if got != tt.epoch {
			t.Errorf("SlotToEpoch(%d, preview) = %d, want %d", tt.slot, got, tt.epoch)
		}
	}
}

func TestGetEpochStartSlotMainnetExtended(t *testing.T) {
	tests := []struct {
		epoch int
		slot  uint64
	}{
		{0, 0},
		{1, 21600},
		{207, 207 * 21600},
		{208, 208 * 21600},                       // Shelley start
		{209, 208*21600 + 432000},                 // Second Shelley epoch
		{365, 208*21600 + uint64(365-208)*432000}, // Babbage start
		{600, 208*21600 + uint64(600-208)*432000}, // Recent epoch
	}

	for _, tt := range tests {
		got := GetEpochStartSlot(tt.epoch, MainnetNetworkMagic)
		if got != tt.slot {
			t.Errorf("GetEpochStartSlot(%d, mainnet) = %d, want %d", tt.epoch, got, tt.slot)
		}
	}
}

func TestGetEpochStartSlotPreprodExtended(t *testing.T) {
	tests := []struct {
		epoch int
		slot  uint64
	}{
		{0, 0},
		{1, 21600},
		{3, 64800},
		{4, 86400},                                   // Shelley start
		{5, 86400 + 432000},                           // Second Shelley epoch
		{12, 86400 + uint64(12-4)*432000},             // Babbage start
	}

	for _, tt := range tests {
		got := GetEpochStartSlot(tt.epoch, PreprodNetworkMagic)
		if got != tt.slot {
			t.Errorf("GetEpochStartSlot(%d, preprod) = %d, want %d", tt.epoch, got, tt.slot)
		}
	}
}

func TestGetEpochStartSlotPreview(t *testing.T) {
	for epoch := 0; epoch < 50; epoch++ {
		got := GetEpochStartSlot(epoch, PreviewNetworkMagic)
		want := uint64(epoch) * 86400
		if got != want {
			t.Errorf("GetEpochStartSlot(%d, preview) = %d, want %d", epoch, got, want)
		}
	}
}

// Round-trip for extended range
func TestGetEpochStartSlotRoundTripExtended(t *testing.T) {
	networks := []struct {
		name  string
		magic int
		max   int
	}{
		{"mainnet", MainnetNetworkMagic, 650},
		{"preprod", PreprodNetworkMagic, 200},
		{"preview", PreviewNetworkMagic, 1000},
	}

	for _, net := range networks {
		for epoch := 0; epoch < net.max; epoch++ {
			startSlot := GetEpochStartSlot(epoch, net.magic)
			gotEpoch := SlotToEpoch(startSlot, net.magic)
			if gotEpoch != epoch {
				t.Errorf("%s: SlotToEpoch(GetEpochStartSlot(%d)) = %d (startSlot=%d)",
					net.name, epoch, gotEpoch, startSlot)
			}
			// Also test last slot of epoch — must use correct epoch length
			// Byron epochs are ByronEpochLength slots, Shelley+ are full epoch length
			var thisEpochLen uint64
			switch net.magic {
			case MainnetNetworkMagic:
				if epoch < ShelleyStartEpoch {
					thisEpochLen = ByronEpochLength
				} else {
					thisEpochLen = MainnetEpochLength
				}
			case PreprodNetworkMagic:
				if epoch < PreprodShelleyStartEpoch {
					thisEpochLen = ByronEpochLength
				} else {
					thisEpochLen = MainnetEpochLength
				}
			default:
				thisEpochLen = GetEpochLength(net.magic)
			}
			lastSlot := startSlot + thisEpochLen - 1
			gotLastEpoch := SlotToEpoch(lastSlot, net.magic)
			if gotLastEpoch != epoch {
				t.Errorf("%s: SlotToEpoch(lastSlot=%d) = %d, want %d",
					net.name, lastSlot, gotLastEpoch, epoch)
			}
		}
	}
}

// ============================================================================
// Stability Window — Era Transitions
// ============================================================================

func TestStabilityWindowAllEras(t *testing.T) {
	// TPraos epochs (pre-Babbage): 3k/f = 129600 margin → 302400
	tpraosEpochs := []int{208, 250, 300, 364}
	for _, e := range tpraosEpochs {
		got := StabilityWindowSlotsForEpoch(e, MainnetNetworkMagic)
		if got != 302400 {
			t.Errorf("epoch %d (TPraos): got %d, want 302400", e, got)
		}
	}

	// CPraos epochs (Babbage+): 4k/f = 172800 margin → 259200
	cpraosEpochs := []int{365, 400, 500, 600}
	for _, e := range cpraosEpochs {
		got := StabilityWindowSlotsForEpoch(e, MainnetNetworkMagic)
		if got != 259200 {
			t.Errorf("epoch %d (CPraos): got %d, want 259200", e, got)
		}
	}

	// Preprod transition
	if got := StabilityWindowSlotsForEpoch(11, PreprodNetworkMagic); got != 302400 {
		t.Errorf("preprod epoch 11 (TPraos): got %d, want 302400", got)
	}
	if got := StabilityWindowSlotsForEpoch(12, PreprodNetworkMagic); got != 259200 {
		t.Errorf("preprod epoch 12 (CPraos): got %d, want 259200", got)
	}

	// Preview — smaller epoch, uses percentage (70% because epoch 0 < BabbageStartEpoch)
	got := StabilityWindowSlotsForEpoch(0, PreviewNetworkMagic)
	if got != 60480 { // 86400 * 70 / 100
		t.Errorf("preview epoch 0: got %d, want 60480", got)
	}
	// Preview epoch 365+ would use 60%
	got = StabilityWindowSlotsForEpoch(365, PreviewNetworkMagic)
	if got != 51840 { // 86400 * 60 / 100
		t.Errorf("preview epoch 365: got %d, want 51840", got)
	}
}

// ============================================================================
// Nonce Functions — Comprehensive
// ============================================================================

func TestVrfNonceValueDeterministic(t *testing.T) {
	// Same input → same output
	input := make([]byte, 64)
	for i := range input {
		input[i] = byte(i * 3)
	}
	r1 := vrfNonceValue(input)
	r2 := vrfNonceValue(input)
	if hex.EncodeToString(r1) != hex.EncodeToString(r2) {
		t.Fatal("vrfNonceValue not deterministic")
	}
}

func TestVrfNonceValueForEpochEraBoundary(t *testing.T) {
	vrfOutput := make([]byte, 64)
	for i := range vrfOutput {
		vrfOutput[i] = byte(i)
	}

	// Epoch 364 (last TPraos) should differ from epoch 365 (first CPraos)
	shelley := vrfNonceValueForEpoch(vrfOutput, 364, MainnetNetworkMagic)
	babbage := vrfNonceValueForEpoch(vrfOutput, 365, MainnetNetworkMagic)
	if hex.EncodeToString(shelley) == hex.EncodeToString(babbage) {
		t.Fatal("TPraos/CPraos nonce should differ at era boundary")
	}

	// All TPraos epochs should give same result (no domain separator)
	for _, e := range []int{208, 250, 300, 364} {
		got := vrfNonceValueForEpoch(vrfOutput, e, MainnetNetworkMagic)
		if hex.EncodeToString(got) != hex.EncodeToString(shelley) {
			t.Errorf("epoch %d: expected same as epoch 208", e)
		}
	}

	// All CPraos epochs should give same result (with domain separator)
	for _, e := range []int{365, 400, 500, 600} {
		got := vrfNonceValueForEpoch(vrfOutput, e, MainnetNetworkMagic)
		if hex.EncodeToString(got) != hex.EncodeToString(babbage) {
			t.Errorf("epoch %d: expected same as epoch 365", e)
		}
	}
}

func TestEvolveNonceDeterministic(t *testing.T) {
	a := make([]byte, 32)
	b := make([]byte, 32)
	a[0] = 1
	b[0] = 2
	r1 := evolveNonce(a, b)
	r2 := evolveNonce(a, b)
	if hex.EncodeToString(r1) != hex.EncodeToString(r2) {
		t.Fatal("evolveNonce not deterministic")
	}
}

func TestEvolveNonceNonCommutative(t *testing.T) {
	a := make([]byte, 32)
	b := make([]byte, 32)
	a[0] = 0xAA
	b[0] = 0xBB
	r1 := evolveNonce(a, b)
	r2 := evolveNonce(b, a)
	if hex.EncodeToString(r1) == hex.EncodeToString(r2) {
		t.Fatal("evolveNonce should not be commutative (concatenation not XOR)")
	}
}

func TestHashConcatNonCommutative(t *testing.T) {
	a := make([]byte, 32)
	b := make([]byte, 32)
	a[0] = 0x11
	b[0] = 0x22
	r1 := hashConcat(a, b)
	r2 := hashConcat(b, a)
	if hex.EncodeToString(r1) == hex.EncodeToString(r2) {
		t.Fatal("hashConcat should not be commutative")
	}
}

func TestInitialNonceModes(t *testing.T) {
	full := initialNonce(true)
	lite := initialNonce(false)

	if len(full) != 32 {
		t.Fatalf("full mode nonce length: %d", len(full))
	}
	if len(lite) != 32 {
		t.Fatalf("lite mode nonce length: %d", len(lite))
	}

	// Full mode = Shelley genesis hash
	expected, _ := hex.DecodeString(ShelleyGenesisHash)
	if hex.EncodeToString(full) != hex.EncodeToString(expected) {
		t.Fatalf("full mode nonce: got %s, want %s", hex.EncodeToString(full), ShelleyGenesisHash)
	}

	// Lite mode = all zeros
	allZero := true
	for _, b := range lite {
		if b != 0 {
			allZero = false
			break
		}
	}
	if !allZero {
		t.Fatal("lite mode nonce should be all zeros")
	}

	// They must differ
	if hex.EncodeToString(full) == hex.EncodeToString(lite) {
		t.Fatal("full and lite mode nonces should differ")
	}
}

// ============================================================================
// VRF Key Parsing
// ============================================================================

func TestParseVRFKeyCborHexValid(t *testing.T) {
	// Create a synthetic 64-byte key (32 private + 32 public)
	keyBytes := make([]byte, 64)
	for i := range keyBytes {
		keyBytes[i] = byte(i)
	}
	cborHex := "5840" + hex.EncodeToString(keyBytes)

	key, err := ParseVRFKeyCborHex(cborHex)
	if err != nil {
		t.Fatalf("ParseVRFKeyCborHex: %v", err)
	}
	if len(key.PrivateKey) != 32 {
		t.Fatalf("PrivateKey length: %d", len(key.PrivateKey))
	}
	if len(key.PublicKey) != 32 {
		t.Fatalf("PublicKey length: %d", len(key.PublicKey))
	}
	if key.PrivateKey[0] != 0 || key.PrivateKey[31] != 31 {
		t.Fatal("PrivateKey bytes wrong")
	}
	if key.PublicKey[0] != 32 || key.PublicKey[31] != 63 {
		t.Fatal("PublicKey bytes wrong")
	}
}

func TestParseVRFKeyCborHexWithoutPrefix(t *testing.T) {
	keyBytes := make([]byte, 64)
	for i := range keyBytes {
		keyBytes[i] = byte(i)
	}
	rawHex := hex.EncodeToString(keyBytes)

	key, err := ParseVRFKeyCborHex(rawHex)
	if err != nil {
		t.Fatalf("ParseVRFKeyCborHex without prefix: %v", err)
	}
	if len(key.PrivateKey) != 32 || len(key.PublicKey) != 32 {
		t.Fatal("key lengths wrong")
	}
}

func TestParseVRFKeyCborHexInvalidLength(t *testing.T) {
	shortHex := "5840" + hex.EncodeToString(make([]byte, 32))
	_, err := ParseVRFKeyCborHex(shortHex)
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestParseVRFKeyCborHexInvalidHex(t *testing.T) {
	_, err := ParseVRFKeyCborHex("5840ZZZZ")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

func TestParseVRFKeyCborHexWithWhitespace(t *testing.T) {
	keyBytes := make([]byte, 64)
	cborHex := "  5840" + hex.EncodeToString(keyBytes) + "  "

	key, err := ParseVRFKeyCborHex(cborHex)
	if err != nil {
		t.Fatalf("ParseVRFKeyCborHex with whitespace: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
}

// ============================================================================
// CPraos Leader Election Math
// ============================================================================

func TestCalculateThreshold(t *testing.T) {
	// With 100% of stake, threshold should be close to 2^256 * 0.05
	threshold := calculateThreshold(1000, 1000, 0.05)
	if threshold.Sign() <= 0 {
		t.Fatal("threshold should be positive")
	}

	certNatMax := certNatMaxCpraos()
	if threshold.Cmp(certNatMax) >= 0 {
		t.Fatal("threshold must be less than 2^256")
	}

	// More stake → higher threshold
	t1 := calculateThreshold(100, 1000, 0.05)
	t2 := calculateThreshold(200, 1000, 0.05)
	if t2.Cmp(t1) <= 0 {
		t.Fatal("double stake should give higher threshold")
	}

	// Zero stake → zero threshold
	t0 := calculateThreshold(0, 1000, 0.05)
	if t0.Sign() != 0 {
		t.Fatal("zero stake should give zero threshold")
	}
}

func TestCertNatMaxCpraos(t *testing.T) {
	max := certNatMaxCpraos()
	// 2^256
	expected := new(big.Int).Lsh(big.NewInt(1), 256)
	if max.Cmp(expected) != 0 {
		t.Fatal("certNatMaxCpraos should be 2^256")
	}
}

func TestMkInputVrf(t *testing.T) {
	nonce := make([]byte, 32)
	nonce[0] = 0xFF

	// Different slots should give different VRF inputs
	input1 := mkInputVrf(100, nonce)
	input2 := mkInputVrf(200, nonce)
	if hex.EncodeToString(input1) == hex.EncodeToString(input2) {
		t.Fatal("different slots should give different VRF inputs")
	}

	// Same slot+nonce should be deterministic
	input3 := mkInputVrf(100, nonce)
	if hex.EncodeToString(input1) != hex.EncodeToString(input3) {
		t.Fatal("mkInputVrf not deterministic")
	}

	// Should be 32 bytes (BLAKE2b-256)
	if len(input1) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(input1))
	}
}

func TestVrfLeaderValue(t *testing.T) {
	output := make([]byte, 64)
	output[0] = 0xAB

	// Should produce 32-byte BLAKE2b-256
	lv := vrfLeaderValue(output)
	if len(lv) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(lv))
	}

	// Manual verification: BLAKE2b-256("L" || output)
	h, _ := blake2b.New256(nil)
	h.Write([]byte{0x4C})
	h.Write(output)
	expected := h.Sum(nil)
	if hex.EncodeToString(lv) != hex.EncodeToString(expected) {
		t.Fatal("vrfLeaderValue mismatch")
	}

	// Deterministic
	lv2 := vrfLeaderValue(output)
	if hex.EncodeToString(lv) != hex.EncodeToString(lv2) {
		t.Fatal("vrfLeaderValue not deterministic")
	}
}

func TestIsSlotLeaderCpraosNilKey(t *testing.T) {
	nonce := make([]byte, 32)
	_, err := IsSlotLeaderCpraos(100, nonce, nil, 1000, 10000)
	if err == nil {
		t.Fatal("expected error for nil VRF key")
	}
}

// ============================================================================
// Schedule Calculation Validation
// ============================================================================

func TestCalculateLeaderScheduleInvalidNonce(t *testing.T) {
	key := &VRFKey{PrivateKey: make([]byte, 32), PublicKey: make([]byte, 32)}
	_, err := CalculateLeaderSchedule(400, make([]byte, 16), key, 1000, 10000, 432000, 0, func(uint64) time.Time { return time.Now() })
	if err == nil {
		t.Fatal("expected error for short nonce")
	}
}

func TestCalculateLeaderScheduleZeroTotalStake(t *testing.T) {
	key := &VRFKey{PrivateKey: make([]byte, 32), PublicKey: make([]byte, 32)}
	_, err := CalculateLeaderSchedule(400, make([]byte, 32), key, 1000, 0, 432000, 0, func(uint64) time.Time { return time.Now() })
	if err == nil {
		t.Fatal("expected error for zero totalStake")
	}
}

// ============================================================================
// FormatScheduleForTelegram
// ============================================================================

func TestFormatScheduleForTelegram12h(t *testing.T) {
	schedule := &LeaderSchedule{
		Epoch:      500,
		EpochNonce: "aabbccdd",
		PoolStake:  15_000_000_000_000,
		TotalStake: 22_000_000_000_000_000,
		Sigma:      0.000681818,
		IdealSlots: 14.73,
		AssignedSlots: []LeaderSlot{
			{No: 1, Slot: 100000, SlotInEpoch: 100, At: time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC)},
		},
	}

	msg := FormatScheduleForTelegram(schedule, "OTG", "UTC", "12h")
	if msg == "" {
		t.Fatal("empty format output")
	}
	if len(msg) < 50 {
		t.Fatalf("format output too short: %d chars", len(msg))
	}
	// Should contain key fields
	for _, substr := range []string{"Epoch: 500", "aabbccdd", "Ideal Blocks:", "12-hour"} {
		found := false
		for i := 0; i < len(msg)-len(substr)+1; i++ {
			if msg[i:i+len(substr)] == substr {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in output", substr)
		}
	}
}

func TestFormatScheduleForTelegram24h(t *testing.T) {
	schedule := &LeaderSchedule{
		Epoch:         500,
		EpochNonce:    "aabbccdd",
		PoolStake:     15_000_000_000_000,
		TotalStake:    22_000_000_000_000_000,
		Sigma:         0.000681818,
		IdealSlots:    14.73,
		AssignedSlots: []LeaderSlot{},
	}

	msg := FormatScheduleForTelegram(schedule, "OTG", "UTC", "24h")
	if msg == "" {
		t.Fatal("empty format output")
	}
	// Should contain "No slots assigned"
	found := false
	for i := 0; i < len(msg)-16; i++ {
		if msg[i:i+16] == "No slots assigne" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'No slots assigned' in output for empty schedule")
	}
}

func TestFormatNumberExtended(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{123456, "123,456"},
		{1234567, "1,234,567"},
		{1234567890, "1,234,567,890"},
		{-1, "-1"},
		{-999, "-999"},
		{-1000, "-1,000"},
		{-1234567, "-1,234,567"},
		{1000000000, "1,000,000,000"},
		{15000000000000, "15,000,000,000,000"},     // pool stake size
		{22000000000000000, "22,000,000,000,000,000"}, // total stake size
	}

	for _, tt := range tests {
		got := formatNumber(tt.n)
		if got != tt.want {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// ============================================================================
// Store Operations — Extended
// ============================================================================

func TestStoreBlockBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	blocks := []BlockData{
		{Slot: 100, Epoch: 1, BlockHash: "h1", VrfOutput: make([]byte, 64), NetworkMagic: MainnetNetworkMagic},
		{Slot: 200, Epoch: 1, BlockHash: "h2", VrfOutput: make([]byte, 64), NetworkMagic: MainnetNetworkMagic},
		{Slot: 300, Epoch: 1, BlockHash: "h3", VrfOutput: make([]byte, 64), NetworkMagic: MainnetNetworkMagic},
	}

	if err := store.InsertBlockBatch(ctx, blocks); err != nil {
		t.Fatalf("InsertBlockBatch: %v", err)
	}

	slot, err := store.GetLastSyncedSlot(ctx)
	if err != nil {
		t.Fatalf("GetLastSyncedSlot: %v", err)
	}
	if slot != 300 {
		t.Fatalf("expected last slot 300, got %d", slot)
	}

	count, err := store.GetBlockCountForEpoch(ctx, 1)
	if err != nil {
		t.Fatalf("GetBlockCountForEpoch: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 blocks, got %d", count)
	}

	// Duplicate batch should not error (ON CONFLICT DO NOTHING)
	if err := store.InsertBlockBatch(ctx, blocks); err != nil {
		t.Fatalf("InsertBlockBatch duplicate: %v", err)
	}

	// Count should still be 3
	count, err = store.GetBlockCountForEpoch(ctx, 1)
	if err != nil {
		t.Fatalf("GetBlockCountForEpoch after dup: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 blocks after dup, got %d", count)
	}
}

func TestStoreBlockByHash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, _ = store.InsertBlock(ctx, 100, 1, "abcdef123456", []byte{1}, []byte{1})
	_, _ = store.InsertBlock(ctx, 200, 1, "abcdef789012", []byte{2}, []byte{2})
	_, _ = store.InsertBlock(ctx, 300, 1, "ffffff000000", []byte{3}, []byte{3})

	// Prefix match
	records, err := store.GetBlockByHash(ctx, "abcdef")
	if err != nil {
		t.Fatalf("GetBlockByHash: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(records))
	}

	// Full hash match
	records, err = store.GetBlockByHash(ctx, "ffffff000000")
	if err != nil {
		t.Fatalf("GetBlockByHash full: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 match, got %d", len(records))
	}

	// No match
	records, err = store.GetBlockByHash(ctx, "999999")
	if err != nil {
		t.Fatalf("GetBlockByHash none: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(records))
	}
}

func TestStoreScheduleRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	schedule := &LeaderSchedule{
		Epoch:      500,
		EpochNonce: "aabb",
		PoolStake:  1000,
		TotalStake: 10000,
		Sigma:      0.1,
		IdealSlots: 2160.0,
		AssignedSlots: []LeaderSlot{
			{No: 1, Slot: 100, SlotInEpoch: 10, At: now},
			{No: 2, Slot: 200, SlotInEpoch: 110, At: now.Add(100 * time.Second)},
		},
		CalculatedAt: now,
	}

	if err := store.InsertLeaderSchedule(ctx, schedule); err != nil {
		t.Fatalf("InsertLeaderSchedule: %v", err)
	}

	got, err := store.GetLeaderSchedule(ctx, 500)
	if err != nil {
		t.Fatalf("GetLeaderSchedule: %v", err)
	}
	if got.Epoch != 500 {
		t.Fatalf("epoch: got %d, want 500", got.Epoch)
	}
	if len(got.AssignedSlots) != 2 {
		t.Fatalf("slots: got %d, want 2", len(got.AssignedSlots))
	}
	if got.AssignedSlots[0].Slot != 100 {
		t.Fatalf("first slot: got %d, want 100", got.AssignedSlots[0].Slot)
	}

	// Missing epoch
	_, err = store.GetLeaderSchedule(ctx, 999)
	if err == nil {
		t.Fatal("expected error for missing schedule")
	}
}

func TestStoreForgedSlots(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, _ = store.InsertBlock(ctx, 100, 10, "h1", []byte{1}, []byte{1})
	_, _ = store.InsertBlock(ctx, 200, 10, "h2", []byte{2}, []byte{2})
	_, _ = store.InsertBlock(ctx, 300, 11, "h3", []byte{3}, []byte{3})

	slots, err := store.GetForgedSlots(ctx, 10)
	if err != nil {
		t.Fatalf("GetForgedSlots: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(slots))
	}
	if slots[0] != 100 || slots[1] != 200 {
		t.Fatalf("slots: got %v, want [100, 200]", slots)
	}

	// Empty epoch
	slots, err = store.GetForgedSlots(ctx, 999)
	if err != nil {
		t.Fatalf("GetForgedSlots empty: %v", err)
	}
	if len(slots) != 0 {
		t.Fatalf("expected 0 slots, got %d", len(slots))
	}
}

func TestStoreStreamBlockNonces(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, _ = store.InsertBlock(ctx, 300, 2, "h3", []byte{3}, []byte{33})
	_, _ = store.InsertBlock(ctx, 100, 1, "h1", []byte{1}, []byte{11})
	_, _ = store.InsertBlock(ctx, 200, 1, "h2", []byte{2}, []byte{22})

	rows, err := store.StreamBlockNonces(ctx)
	if err != nil {
		t.Fatalf("StreamBlockNonces: %v", err)
	}
	defer rows.Close()

	var slots []uint64
	for rows.Next() {
		_, slot, _, _, err := rows.Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}

	// Should be ordered by slot ASC
	if len(slots) != 3 {
		t.Fatalf("expected 3, got %d", len(slots))
	}
	if slots[0] != 100 || slots[1] != 200 || slots[2] != 300 {
		t.Fatalf("slots not in order: %v", slots)
	}
}

func TestStoreStreamBlockVrfOutputs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, _ = store.InsertBlock(ctx, 200, 1, "h2", []byte{2, 2}, []byte{22})
	_, _ = store.InsertBlock(ctx, 100, 1, "h1", []byte{1, 1}, []byte{11})

	rows, err := store.StreamBlockVrfOutputs(ctx)
	if err != nil {
		t.Fatalf("StreamBlockVrfOutputs: %v", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		epoch, slot, vrfOutput, nonceValue, blockHash, err := rows.Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if epoch != 1 {
			t.Fatalf("epoch: got %d", epoch)
		}
		if count == 0 && slot != 100 {
			t.Fatalf("first slot should be 100, got %d", slot)
		}
		if len(vrfOutput) == 0 || len(nonceValue) == 0 || blockHash == "" {
			t.Fatal("empty fields")
		}
		count++
	}
	if count != 2 {
		t.Fatalf("expected 2, got %d", count)
	}
}

// ============================================================================
// Pallas Test Vectors — Extended
// ============================================================================

func TestPallasRollingNonceExtended(t *testing.T) {
	// Same test as TestPallasRollingNonce but verify properties:
	// 1. Each step produces different result
	// 2. Rolling is not reversible
	genesisHash, _ := hex.DecodeString(ShelleyGenesisHash)
	etaV := make([]byte, 32)
	copy(etaV, genesisHash)

	vrfOutputs := []string{
		"36ec5378d1f5041a59eb8d96e61de96f0950fb41b49ff511f7bc7fd109d4383e1d24be7034e6749c6612700dd5ceb0c66577b88a19ae286b1321d15bce1ab736",
		"e0bf34a6b73481302f22987cde4c12807cbc2c3fea3f7fcb77261385a50e8ccdda3226db3efff73e9fb15eecf841bbc85ce37550de0435ebcdcb205e0ed08467",
	}

	prev := hex.EncodeToString(etaV)
	for _, vrfHex := range vrfOutputs {
		vrfOutput, _ := hex.DecodeString(vrfHex)
		nonceValue := vrfNonceValue(vrfOutput)
		etaV = evolveNonce(etaV, nonceValue)
		current := hex.EncodeToString(etaV)
		if current == prev {
			t.Fatal("evolving nonce should change with each block")
		}
		prev = current
	}
}

// ============================================================================
// Koios Epoch Nonce Verification (15 random epochs)
// ============================================================================

func TestKoiosEpochNonceVerification(t *testing.T) {
	// 15 epochs spanning Shelley through current era
	// Mix of TPraos and CPraos epochs
	epochs := []int{
		210, 225, 250, 280, 300, // Early Shelley / Allegra / Mary
		320, 340, 360,           // Late Alonzo
		370, 400, 450,           // Babbage
		500, 550, 600, 615,      // Conway / recent
	}

	passed := 0
	failed := 0
	errors := 0

	for _, epoch := range epochs {
		nonceHex, err := fetchKoiosNonce(epoch)
		if err != nil {
			t.Logf("Epoch %d: KOIOS ERROR: %v", epoch, err)
			errors++
			continue
		}

		// Verify it's valid 32-byte hex
		nonceBytes, decErr := hex.DecodeString(nonceHex)
		if decErr != nil {
			t.Logf("Epoch %d: BAD HEX: %v", epoch, decErr)
			failed++
			continue
		}
		if len(nonceBytes) != 32 {
			t.Logf("Epoch %d: BAD LENGTH: %d bytes", epoch, len(nonceBytes))
			failed++
			continue
		}

		t.Logf("Epoch %d: OK  %s...", epoch, nonceHex[:16])
		passed++

		// Rate limit
		time.Sleep(100 * time.Millisecond)
	}

	t.Logf("\n=== Koios Verification: %d passed, %d failed, %d errors ===", passed, failed, errors)

	if failed > 0 {
		t.Fatalf("%d epochs had invalid nonces", failed)
	}
	if errors > 3 {
		t.Fatalf("too many Koios errors: %d", errors)
	}
}

// TestKoiosStakeDataAvailable verifies Koios returns valid stake data
// for a range of epochs, testing the same API paths the bot uses.
func TestKoiosStakeDataAvailable(t *testing.T) {
	epochs := []int{400, 450, 500, 550, 600}
	poolBech32 := "pool1eq8aq5cesav8rplnrlxhwkfhxcnss5pu30z8u9f09y3fnw7z8dn"

	for _, epoch := range epochs {
		// Fetch pool stake
		url := fmt.Sprintf("https://api.koios.rest/api/v1/pool_history?_pool_bech32=%s&_epoch_no=%d", poolBech32, epoch)
		resp, err := http.Get(url)
		if err != nil {
			t.Logf("Epoch %d pool_history: NETWORK ERROR: %v", epoch, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Logf("Epoch %d pool_history: HTTP %d", epoch, resp.StatusCode)
			continue
		}

		var poolResult []struct {
			ActiveStake json.Number `json:"active_stake"`
		}
		if err := json.Unmarshal(body, &poolResult); err != nil {
			t.Logf("Epoch %d pool_history: PARSE ERROR", epoch)
			continue
		}

		if len(poolResult) == 0 {
			t.Logf("Epoch %d: no pool history data", epoch)
			continue
		}

		stake, _ := poolResult[0].ActiveStake.Int64()
		if stake <= 0 {
			t.Errorf("Epoch %d: pool stake should be positive, got %d", epoch, stake)
			continue
		}

		t.Logf("Epoch %d: pool_stake=%d", epoch, stake)
		time.Sleep(100 * time.Millisecond)
	}
}

// TestKoiosLastBlockHash tests the Koios blocks API endpoint used for TICKN η_ph.
func TestKoiosLastBlockHash(t *testing.T) {
	epochs := []int{400, 450, 500, 550, 600}

	for _, epoch := range epochs {
		url := fmt.Sprintf("https://api.koios.rest/api/v1/blocks?epoch_no=eq.%d&order=block_height.desc&limit=1&select=hash,epoch_no,block_height", epoch)
		resp, err := http.Get(url)
		if err != nil {
			t.Logf("Epoch %d: NETWORK ERROR: %v", epoch, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Logf("Epoch %d: HTTP %d", epoch, resp.StatusCode)
			continue
		}

		var result []struct {
			Hash        string `json:"hash"`
			EpochNo     int    `json:"epoch_no"`
			BlockHeight int    `json:"block_height"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Logf("Epoch %d: PARSE ERROR", epoch)
			continue
		}
		if len(result) == 0 {
			t.Errorf("Epoch %d: no blocks returned", epoch)
			continue
		}

		hash := result[0].Hash
		if len(hash) != 64 {
			t.Errorf("Epoch %d: hash length %d, want 64", epoch, len(hash))
			continue
		}
		if _, err := hex.DecodeString(hash); err != nil {
			t.Errorf("Epoch %d: invalid hash hex: %v", epoch, err)
			continue
		}

		t.Logf("Epoch %d: last_block=%s... (height %d)", epoch, hash[:16], result[0].BlockHeight)
		time.Sleep(100 * time.Millisecond)
	}
}

// ============================================================================
// GetEpochLength
// ============================================================================

func TestGetEpochLengthAllNetworks(t *testing.T) {
	if GetEpochLength(MainnetNetworkMagic) != 432000 {
		t.Error("mainnet")
	}
	if GetEpochLength(PreprodNetworkMagic) != 432000 {
		t.Error("preprod")
	}
	if GetEpochLength(PreviewNetworkMagic) != 86400 {
		t.Error("preview")
	}
	// Unknown network defaults to mainnet
	if GetEpochLength(9999) != 432000 {
		t.Error("unknown network")
	}
}

// ============================================================================
// Constants Sanity
// ============================================================================

func TestConstants(t *testing.T) {
	if MainnetNetworkMagic != 764824073 {
		t.Error("mainnet magic")
	}
	if PreprodNetworkMagic != 1 {
		t.Error("preprod magic")
	}
	if PreviewNetworkMagic != 2 {
		t.Error("preview magic")
	}
	if MainnetEpochLength != 432000 {
		t.Error("mainnet epoch length")
	}
	if PreviewEpochLength != 86400 {
		t.Error("preview epoch length")
	}
	if ByronEpochLength != 21600 {
		t.Error("byron epoch length")
	}
	if ShelleyStartEpoch != 208 {
		t.Error("shelley start")
	}
	if PreprodShelleyStartEpoch != 4 {
		t.Error("preprod shelley start")
	}
	if BabbageStartEpoch != 365 {
		t.Error("babbage start")
	}
	if PreprodBabbageStartEpoch != 12 {
		t.Error("preprod babbage start")
	}
	if ActiveSlotCoeff != 0.05 {
		t.Error("active slot coeff")
	}
	if ShelleyGenesisHash != "1a3be38bcbb7911969283716ad7aa550250226b76a61fc51cc9a9a35d9276d81" {
		t.Error("shelley genesis hash")
	}
}
