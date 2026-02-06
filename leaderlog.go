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
	"time"

	"github.com/blinklabs-io/gouroboros/vrf"
	"golang.org/x/crypto/blake2b"
)

const (
	// ActiveSlotCoeff is the probability a slot has a leader (5% on mainnet)
	ActiveSlotCoeff = 0.05

	// MainnetEpochLength is slots per epoch on mainnet
	MainnetEpochLength = 432000

	// PreviewEpochLength is slots per epoch on preview
	PreviewEpochLength = 86400

	// ShelleyStartEpoch is the first Shelley epoch
	ShelleyStartEpoch = 208
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
	AssignedSlots []LeaderSlot `json:"assignedSlots"`
	CalculatedAt  time.Time    `json:"calculatedAt"`
}

// VRFKey holds the parsed VRF signing key
type VRFKey struct {
	PrivateKey []byte // 32 bytes
	PublicKey  []byte // 32 bytes
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
	if len(envelope.CborHex) < 4 {
		return nil, fmt.Errorf("cborHex too short")
	}

	// Strip CBOR prefix and decode
	keyHex := envelope.CborHex[4:] // Remove "5840" prefix
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key hex: %w", err)
	}

	if len(keyBytes) != 64 {
		return nil, fmt.Errorf("expected 64 bytes, got %d", len(keyBytes))
	}

	return &VRFKey{
		PrivateKey: keyBytes[:32],
		PublicKey:  keyBytes[32:],
	}, nil
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

// GetEpochStartSlot calculates the first slot of an epoch
// For mainnet, accounts for Byron era offset
func GetEpochStartSlot(epoch int, networkMagic int) uint64 {
	if networkMagic == 764824073 { // mainnet
		// Byron had 208 epochs, Shelley started at slot 4492800
		// (208 * 21600 = 4,492,800 Byron slots, each was 20s)
		shelleyEpoch := epoch - ShelleyStartEpoch
		return 4492800 + uint64(shelleyEpoch)*MainnetEpochLength
	}
	// Preview/preprod - epochs start from 0
	epochLength := uint64(MainnetEpochLength)
	if networkMagic == 2 { // preview
		epochLength = PreviewEpochLength
	}
	return uint64(epoch) * epochLength
}

// GetEpochLength returns the epoch length for a network
func GetEpochLength(networkMagic int) uint64 {
	if networkMagic == 2 { // preview
		return PreviewEpochLength
	}
	return MainnetEpochLength
}

// FormatScheduleForTelegram creates a Telegram message from a schedule
func FormatScheduleForTelegram(schedule *LeaderSchedule, poolName, timezone string) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}

	poolStakeADA := int64(schedule.PoolStake / 1_000_000)
	totalStakeADA := int64(schedule.TotalStake / 1_000_000)
	performance := 0.0
	if schedule.IdealSlots > 0 {
		performance = float64(len(schedule.AssignedSlots)) / schedule.IdealSlots * 100
	}

	msg := fmt.Sprintf("🦆 %s Leader Schedule\n\n", poolName)
	msg += fmt.Sprintf("Epoch: %d\n", schedule.Epoch)
	msg += fmt.Sprintf("Pool Stake: %s ADA\n", formatNumber(poolStakeADA))
	msg += fmt.Sprintf("Network Stake: %s ADA\n\n", formatNumber(totalStakeADA))
	msg += fmt.Sprintf("Assigned Slots: %d\n", len(schedule.AssignedSlots))
	msg += fmt.Sprintf("Expected: %.2f\n", schedule.IdealSlots)
	msg += fmt.Sprintf("Performance: %.2f%%\n\n", performance)

	if len(schedule.AssignedSlots) > 0 {
		msg += fmt.Sprintf("Schedule (%s):\n", timezone)
		for _, slot := range schedule.AssignedSlots {
			localTime := slot.At.In(loc)
			msg += fmt.Sprintf("  %s - Slot %d\n", localTime.Format("01/02 15:04:05"), slot.Slot)
		}
	} else {
		msg += "No slots assigned this epoch.\n"
	}

	msg += "\nQuack! 🦆"
	return msg
}
