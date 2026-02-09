package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

func TestEpoch612LeaderSchedule(t *testing.T) {
	dbPass := os.Getenv("GODUCKBOT_DB_PASSWORD")
	if dbPass == "" {
		t.Skip("GODUCKBOT_DB_PASSWORD not set, skipping integration test")
	}
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	dbPort := os.Getenv("DB_PORT")
	if dbPort == "" {
		dbPort = "5433"
	}

	connStr := fmt.Sprintf("postgres://goduckbot:%s@%s:%s/goduckbot?sslmode=disable", dbPass, dbHost, dbPort)
	store, err := NewPgStore(connStr)
	if err != nil {
		t.Fatalf("DB connect failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// === Step 1: Compute epoch 612 nonce from chain data ===
	// Stream ALL blocks from Shelley genesis, evolving nonce via XOR (Nonce semigroup).
	// At each epoch's 60% stability window: freeze candidate nonce.
	// At each epoch boundary: TICKN rule — η(new) = η_c XOR η_ph

	overallStart := time.Now()
	nonceStart := time.Now()

	genesisHash, _ := hex.DecodeString(ShelleyGenesisHash)
	etaV := make([]byte, 32) // evolving nonce
	copy(etaV, genesisHash)
	eta0 := make([]byte, 32) // epoch nonce — eta_0(208) = shelley genesis hash
	copy(eta0, genesisHash)
	etaC := make([]byte, 32)          // candidate nonce (frozen at stability window)
	prevHashNonce := make([]byte, 32)  // η_ph — NeutralNonce at Shelley start
	var lastBlockHash string

	currentEpoch := ShelleyStartEpoch
	candidateFrozen := false
	blockCount := 0

	log.Printf("Streaming blocks from Shelley genesis to compute epoch nonces...")

	rows, err := store.pool.Query(ctx,
		"SELECT epoch, slot, nonce_value, block_hash FROM blocks WHERE epoch <= 611 ORDER BY slot ASC")
	if err != nil {
		t.Fatalf("Failed to query blocks: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var epoch int
		var slot uint64
		var nonceValue []byte
		var blockHash string
		if err := rows.Scan(&epoch, &slot, &nonceValue, &blockHash); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}

		// Epoch transition — TICKN rule: η(new) = η_c ⊕ η_ph
		if epoch != currentEpoch {
			if !candidateFrozen {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
			}
			eta0 = xorBytes(etaC, prevHashNonce)
			if lastBlockHash != "" {
				prevHashNonce, _ = hex.DecodeString(lastBlockHash)
			}

			currentEpoch = epoch
			candidateFrozen = false
		}

		// Evolve eta_v via XOR
		etaV = evolveNonce(etaV, nonceValue)
		lastBlockHash = blockHash
		blockCount++

		// Freeze candidate at 60% stability window
		if !candidateFrozen {
			epochStart := GetEpochStartSlot(epoch, MainnetNetworkMagic)
			stabilitySlot := epochStart + StabilityWindowSlots(MainnetNetworkMagic)
			if slot >= stabilitySlot {
				etaC = make([]byte, 32)
				copy(etaC, etaV)
				candidateFrozen = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Row iteration error: %v", err)
	}

	// Final transition: freeze candidate for epoch 611 and compute eta_0(612)
	if !candidateFrozen {
		etaC = make([]byte, 32)
		copy(etaC, etaV)
	}
	if lastBlockHash != "" {
		prevHashNonce, _ = hex.DecodeString(lastBlockHash)
	}
	epoch612Nonce := xorBytes(etaC, prevHashNonce)

	nonceElapsed := time.Since(nonceStart)
	log.Printf("Nonce computation: %d blocks processed in %v", blockCount, nonceElapsed)
	log.Printf("Epoch 612 nonce: %s", hex.EncodeToString(epoch612Nonce))

	// === Step 2: Stake from cardano-cli (hardcoded from stake-snapshot "set") ===
	poolStake := uint64(15221834289676)
	totalStake := uint64(21556514440366017)
	sigma := float64(poolStake) / float64(totalStake)
	log.Printf("Pool stake: %d, Total: %d, Sigma: %.10f", poolStake, totalStake, sigma)

	// === Step 3: Load VRF key ===
	vrfKeyPath := os.Getenv("VRF_KEY_PATH")
	if vrfKeyPath == "" {
		vrfKeyPath = "/tmp/vrf.skey"
	}
	vrfKey, err := ParseVRFKeyFile(vrfKeyPath)
	if err != nil {
		t.Fatalf("VRF key: %v", err)
	}

	// === Step 4: Calculate leader schedule ===
	calcStart := time.Now()
	epochLength := GetEpochLength(MainnetNetworkMagic)
	epochStartSlot := GetEpochStartSlot(612, MainnetNetworkMagic)
	slotToTimeFn := makeSlotToTime(MainnetNetworkMagic)

	log.Printf("Calculating epoch 612 schedule (%d slots)...", epochLength)

	schedule, err := CalculateLeaderSchedule(
		612, epoch612Nonce, vrfKey,
		poolStake, totalStake,
		epochLength, epochStartSlot, slotToTimeFn,
	)
	if err != nil {
		t.Fatalf("Schedule calculation failed: %v", err)
	}

	calcElapsed := time.Since(calcStart)
	totalElapsed := time.Since(overallStart)

	// === Results ===
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	fmt.Println("\n=== Epoch 612 Leader Schedule (OTG / Star Forge) ===")
	fmt.Printf("Epoch Nonce:     %s\n", hex.EncodeToString(epoch612Nonce))
	fmt.Printf("Pool Stake:      %d lovelace (%.2f ADA)\n", poolStake, float64(poolStake)/1e6)
	fmt.Printf("Total Stake:     %d lovelace\n", totalStake)
	fmt.Printf("Sigma:           %.10f\n", schedule.Sigma)
	fmt.Printf("Ideal Slots:     %.2f\n", schedule.IdealSlots)
	fmt.Printf("Assigned Slots:  %d\n\n", len(schedule.AssignedSlots))

	for _, s := range schedule.AssignedSlots {
		localTime := s.At.In(loc)
		fmt.Printf("  #%-3d Slot %-12d (epoch slot %-6d)  %s\n",
			s.No, s.Slot, s.SlotInEpoch, localTime.Format("01/02 03:04:05 PM"))
	}

	fmt.Printf("\n--- Timing ---\n")
	fmt.Printf("Nonce computation:  %v (%d blocks)\n", nonceElapsed, blockCount)
	fmt.Printf("Schedule calc:      %v (432,000 slots)\n", calcElapsed)
	fmt.Printf("Total:              %v\n", totalElapsed)
}
