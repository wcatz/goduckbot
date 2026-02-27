// leaderlog.go - CPRAOS Leader Schedule Calculation
//
// Validated against cncli for preview and mainnet networks.

package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/blinklabs-io/gouroboros/vrf"
	"golang.org/x/crypto/blake2b"
)

const (
	// ActiveSlotCoeff is the probability a slot has a leader (5% on mainnet)
	ActiveSlotCoeff = 0.05

	// Network magic values
	MainnetNetworkMagic = 764824073
	PreprodNetworkMagic = 1
	PreviewNetworkMagic = 2

	// MainnetEpochLength is slots per epoch on mainnet and preprod
	MainnetEpochLength = 432000

	// PreviewEpochLength is slots per epoch on preview
	PreviewEpochLength = 86400

	// ByronEpochLength is slots per epoch during the Byron era (20s slots)
	ByronEpochLength = 21600

	// ShelleyStartEpoch is the first Shelley epoch on mainnet
	ShelleyStartEpoch = 208

	// PreprodShelleyStartEpoch is the first Shelley epoch on preprod
	PreprodShelleyStartEpoch = 4

	// BabbageStartEpoch is the first Babbage (CPraos) epoch on mainnet (Vasil hard fork)
	BabbageStartEpoch = 365

	// PreprodBabbageStartEpoch is the first Babbage epoch on preprod
	PreprodBabbageStartEpoch = 12

	// ConwayStartEpoch is the first Conway epoch on mainnet (Chang hard fork)
	ConwayStartEpoch = 507

	// PreprodConwayStartEpoch is the first Conway epoch on preprod
	PreprodConwayStartEpoch = 163
)

// LeaderSlot represents an assigned slot in the schedule
type LeaderSlot struct {
	No          int       `json:"no"`
	Slot        uint64    `json:"slot"`
	SlotInEpoch uint64    `json:"slotInEpoch"`
	At          time.Time `json:"at"`
}

// LeaderSchedule represents a full epoch's leader schedule
type LeaderSchedule struct {
	Epoch         int          `json:"epoch"`
	EpochNonce    string       `json:"epochNonce"`
	PoolStake     uint64       `json:"poolStake"`
	TotalStake    uint64       `json:"totalStake"`
	Sigma         float64      `json:"sigma"`
	IdealSlots    float64      `json:"idealSlots"`
	Performance   float64      `json:"performance"`
	AssignedSlots []LeaderSlot `json:"assignedSlots"`
	CalculatedAt  time.Time    `json:"calculatedAt"`
}

// VRFKey holds the parsed VRF signing key in secure memory.
// The key material is allocated via mmap outside the Go heap,
// locked into RAM (mlock), and marked read-only (mprotect).
// Call Close() to zero and release the secure memory.
type VRFKey struct {
	PrivateKey []byte // 32 bytes (read-only, mlock'd, non-swappable)
	PublicKey  []byte // 32 bytes (read-only, mlock'd, non-swappable)
	secureMem  []byte // backing mmap allocation (nil if fallback to heap)
}

// Close zeros and releases the memory backing this key.
func (k *VRFKey) Close() {
	if k.secureMem != nil {
		secureFree(k.secureMem)
		k.secureMem = nil
	} else {
		// Heap fallback: zero key material manually
		for i := range k.PrivateKey {
			k.PrivateKey[i] = 0
		}
		for i := range k.PublicKey {
			k.PublicKey[i] = 0
		}
	}
	k.PrivateKey = nil
	k.PublicKey = nil
}

// newVRFKeyFromBytes creates a VRFKey from 64 raw bytes, using secure memory.
// Falls back to heap allocation if secure memory is unavailable.
func newVRFKeyFromBytes(keyBytes []byte) (*VRFKey, error) {
	if len(keyBytes) != 64 {
		return nil, fmt.Errorf("expected 64 bytes, got %d", len(keyBytes))
	}

	mem, err := secureAlloc(64)
	if err != nil {
		// Fallback: heap allocation (still functional, just not mlock'd)
		priv := make([]byte, 32)
		pub := make([]byte, 32)
		copy(priv, keyBytes[:32])
		copy(pub, keyBytes[32:])
		return &VRFKey{PrivateKey: priv, PublicKey: pub}, nil
	}

	copy(mem, keyBytes)
	// Zero the source material
	for i := range keyBytes {
		keyBytes[i] = 0
	}
	if err := secureReadOnly(mem); err != nil {
		secureFree(mem)
		return nil, fmt.Errorf("mprotect: %w", err)
	}

	return &VRFKey{
		PrivateKey: mem[:32],
		PublicKey:  mem[32:64],
		secureMem:  mem,
	}, nil
}

// ParseVRFKeyFile reads and parses a Cardano vrf.skey file
func ParseVRFKeyFile(path string) (*VRFKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read VRF key file: %w", err)
	}

	var envelope struct {
		Type    string `json:"type"`
		CborHex string `json:"cborHex"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("failed to parse VRF key JSON: %w", err)
	}

	if envelope.Type != "VrfSigningKey_PraosVRF" {
		return nil, fmt.Errorf("unexpected key type: %s", envelope.Type)
	}

	// CBOR hex has "5840" prefix (byte string, 64 bytes)
	if len(envelope.CborHex) < 4 || envelope.CborHex[:4] != "5840" {
		return nil, fmt.Errorf("unexpected CBOR prefix: expected '5840', got %q", envelope.CborHex[:min(4, len(envelope.CborHex))])
	}

	keyHex := envelope.CborHex[4:]
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key hex: %w", err)
	}

	return newVRFKeyFromBytes(keyBytes)
}

// ParseVRFKeyCborHex parses a VRF key directly from its CBOR hex string.
// Accepts with or without the "5840" CBOR prefix.
func ParseVRFKeyCborHex(cborHex string) (*VRFKey, error) {
	cborHex = strings.TrimSpace(cborHex)
	if len(cborHex) >= 4 && cborHex[:4] == "5840" {
		cborHex = cborHex[4:]
	}

	keyBytes, err := hex.DecodeString(cborHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode VRF CBOR hex: %w", err)
	}

	return newVRFKeyFromBytes(keyBytes)
}

// mkInputVrf creates VRF input: BLAKE2b-256(slot || epochNonce)
func mkInputVrf(slot uint64, epochNonce []byte) []byte {
	concat := make([]byte, 8+32)
	binary.BigEndian.PutUint64(concat[:8], slot)
	copy(concat[8:], epochNonce)
	h, _ := blake2b.New256(nil)
	h.Write(concat)
	return h.Sum(nil)
}

// vrfLeaderValue computes CPRAOS leader value: BLAKE2b-256("L" || vrfOutput)
func vrfLeaderValue(vrfOutput []byte) []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte{0x4C}) // "L" prefix
	h.Write(vrfOutput)
	return h.Sum(nil)
}

// certNatMaxCpraos returns 2^256 for CPRAOS threshold comparison
func certNatMaxCpraos() *big.Int {
	result := big.NewInt(1)
	result.Lsh(result, 256)
	return result
}

// calculateThreshold computes CPRAOS threshold: 2^256 * (1 - (1-f)^sigma)
func calculateThreshold(poolStake, totalStake uint64, f float64) *big.Int {
	sigma := float64(poolStake) / float64(totalStake)
	phi := 1.0 - math.Pow(1.0-f, sigma)

	certNatMax := certNatMaxCpraos()
	phiRat := new(big.Rat).SetFloat64(phi)

	threshold := new(big.Int)
	threshold.Mul(certNatMax, phiRat.Num())
	threshold.Div(threshold, phiRat.Denom())

	return threshold
}

// IsSlotLeaderCpraos checks if pool is leader for slot using CPRAOS algorithm
func IsSlotLeaderCpraos(slot uint64, epochNonce []byte, vrfKey *VRFKey, poolStake, totalStake uint64) (bool, error) {
	if vrfKey == nil {
		return false, fmt.Errorf("nil VRF key")
	}

	// Step 1: Create VRF input
	vrfInput := mkInputVrf(slot, epochNonce)

	// Step 2: Generate VRF proof
	_, vrfOutput, err := vrf.Prove(vrfKey.PrivateKey, vrfInput)
	if err != nil {
		return false, fmt.Errorf("VRF prove failed: %w", err)
	}

	// Step 3: Compute leader value (CPRAOS specific)
	leaderValue := vrfLeaderValue(vrfOutput)

	// Step 4: Convert to big.Int
	certLeaderVrf := new(big.Int).SetBytes(leaderValue)

	// Step 5: Calculate threshold
	threshold := calculateThreshold(poolStake, totalStake, ActiveSlotCoeff)

	// Step 6: Pool is leader if certLeaderVrf < threshold
	return certLeaderVrf.Cmp(threshold) < 0, nil
}

// CalculateLeaderSchedule computes the full schedule for an epoch
func CalculateLeaderSchedule(
	epoch int,
	epochNonce []byte,
	vrfKey *VRFKey,
	poolStake, totalStake uint64,
	epochLength uint64,
	epochStartSlot uint64,
	slotToTime func(uint64) time.Time,
) (*LeaderSchedule, error) {

	if len(epochNonce) != 32 {
		return nil, fmt.Errorf("epoch nonce must be 32 bytes, got %d", len(epochNonce))
	}
	if totalStake == 0 {
		return nil, fmt.Errorf("totalStake must be non-zero")
	}

	sigma := float64(poolStake) / float64(totalStake)
	idealSlots := float64(epochLength) * ActiveSlotCoeff * sigma

	schedule := &LeaderSchedule{
		Epoch:        epoch,
		EpochNonce:   hex.EncodeToString(epochNonce),
		PoolStake:    poolStake,
		TotalStake:   totalStake,
		Sigma:        sigma,
		IdealSlots:   idealSlots,
		CalculatedAt: time.Now().UTC(),
	}

	endSlot := epochStartSlot + epochLength
	slotNo := 1

	for slot := epochStartSlot; slot < endSlot; slot++ {
		isLeader, err := IsSlotLeaderCpraos(slot, epochNonce, vrfKey, poolStake, totalStake)
		if err != nil {
			return nil, fmt.Errorf("slot %d: %w", slot, err)
		}

		if isLeader {
			schedule.AssignedSlots = append(schedule.AssignedSlots, LeaderSlot{
				No:          slotNo,
				Slot:        slot,
				SlotInEpoch: slot - epochStartSlot,
				At:          slotToTime(slot),
			})
			slotNo++
		}
	}

	return schedule, nil
}

// GetEpochStartSlot calculates the first slot of an epoch.
// Accounts for Byron era offset on mainnet and preprod.
func GetEpochStartSlot(epoch int, networkMagic int) uint64 {
	switch networkMagic {
	case MainnetNetworkMagic:
		if epoch < ShelleyStartEpoch {
			return uint64(epoch) * ByronEpochLength
		}
		return uint64(ShelleyStartEpoch)*ByronEpochLength +
			uint64(epoch-ShelleyStartEpoch)*MainnetEpochLength
	case PreprodNetworkMagic:
		if epoch < PreprodShelleyStartEpoch {
			return uint64(epoch) * ByronEpochLength
		}
		return uint64(PreprodShelleyStartEpoch)*ByronEpochLength +
			uint64(epoch-PreprodShelleyStartEpoch)*MainnetEpochLength
	case PreviewNetworkMagic:
		return uint64(epoch) * PreviewEpochLength
	default:
		return uint64(epoch) * MainnetEpochLength
	}
}

// GetEpochLength returns the epoch length for a network
func GetEpochLength(networkMagic int) uint64 {
	if networkMagic == PreviewNetworkMagic {
		return PreviewEpochLength
	}
	return MainnetEpochLength
}

// StabilityWindowSlots returns the nonce stabilisation window for the current era (Conway).
// Use StabilityWindowSlotsForEpoch for historical computation.
func StabilityWindowSlots(networkMagic int) uint64 {
	return StabilityWindowSlotsForEpoch(ConwayStartEpoch, networkMagic)
}

// StabilityWindowSlotsForEpoch returns the era-correct randomness stabilisation window.
// This controls WHEN the candidate nonce is frozen during each epoch.
//
// From ouroboros-consensus Node.hs:
//   - Babbage uses computeStabilityWindow (3k/f) for praosRandomnessStabilisationWindow
//     (backward compat, erratum 17.3 in Shelley ledger specs)
//   - Conway+ uses computeRandomnessStabilisationWindow (4k/f)
//
// Shelley-Babbage: 3k/f = 129600 margin → freeze at 302400 (70% of epoch)
// Conway+:         4k/f = 172800 margin → freeze at 259200 (60% of epoch)
func StabilityWindowSlotsForEpoch(epoch, networkMagic int) uint64 {
	epochLen := GetEpochLength(networkMagic)
	conwayStart := ConwayStartEpoch
	if networkMagic == PreprodNetworkMagic {
		conwayStart = PreprodConwayStartEpoch
	}

	var margin uint64
	if epoch >= conwayStart {
		margin = 172800 // 4k/f (Conway+: computeRandomnessStabilisationWindow)
	} else {
		margin = 129600 // 3k/f (Shelley-Babbage: computeStabilityWindow)
	}

	if epochLen <= margin {
		if epoch >= conwayStart {
			return epochLen * 60 / 100
		}
		return epochLen * 70 / 100
	}
	return epochLen - margin
}

// SlotToEpoch returns the epoch number for a given slot.
// Accounts for Byron era offset on mainnet and preprod.
func SlotToEpoch(slot uint64, networkMagic int) int {
	switch networkMagic {
	case MainnetNetworkMagic:
		byronSlots := uint64(ShelleyStartEpoch) * ByronEpochLength
		if slot < byronSlots {
			return int(slot / ByronEpochLength)
		}
		return ShelleyStartEpoch + int((slot-byronSlots)/MainnetEpochLength)
	case PreprodNetworkMagic:
		byronSlots := uint64(PreprodShelleyStartEpoch) * ByronEpochLength
		if slot < byronSlots {
			return int(slot / ByronEpochLength)
		}
		return PreprodShelleyStartEpoch + int((slot-byronSlots)/MainnetEpochLength)
	case PreviewNetworkMagic:
		return int(slot / PreviewEpochLength)
	default:
		return int(slot / MainnetEpochLength)
	}
}

// FormatScheduleForTelegram creates a Telegram message from a schedule.
// timeFormat should be "12h" or "24h" (defaults to "12h").
func FormatScheduleForTelegram(schedule *LeaderSchedule, poolName, timezone, timeFormat string) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}

	timeFmt := "01/02/2006 03:04:05 PM"
	fmtLabel := "12-hour"
	if timeFormat == "24h" {
		timeFmt = "01/02/2006 15:04:05"
		fmtLabel = "24-hour"
	}

	performance := 0.0
	if schedule.IdealSlots > 0 {
		performance = float64(len(schedule.AssignedSlots)) / schedule.IdealSlots * 100
	}

	msg := fmt.Sprintf("Epoch: %d\n", schedule.Epoch)
	msg += fmt.Sprintf("Nonce: %s\n", schedule.EpochNonce)
	msg += fmt.Sprintf("Pool Active Stake:  %s\u20B3\n", formatADA(schedule.PoolStake))
	msg += fmt.Sprintf("Network Active Stake: %s\u20B3\n", formatADA(schedule.TotalStake))
	msg += fmt.Sprintf("Ideal Blocks: %.2f\n\n", schedule.IdealSlots)

	if len(schedule.AssignedSlots) > 0 {
		msg += fmt.Sprintf("Assigned Slots in %s Format (%s):\n", fmtLabel, timezone)
		for _, slot := range schedule.AssignedSlots {
			localTime := slot.At.In(loc)
			msg += fmt.Sprintf("  %s - Slot: %d  - B: %d\n",
				localTime.Format(timeFmt), slot.SlotInEpoch, slot.No)
		}
	} else {
		msg += "No slots assigned this epoch.\n"
	}

	msg += fmt.Sprintf("\nTotal Scheduled Blocks: %d\n", len(schedule.AssignedSlots))
	msg += fmt.Sprintf("Assigned Epoch Performance: %.2f %%\n", performance)
	return msg
}
