package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestEpoch467Leaderlog calculates the leader schedule for epoch 467
// using Koios for nonce and stake data, and the production VRF key.
func TestEpoch467Leaderlog(t *testing.T) {
	// Production VRF key (from configmap)
	vrfKeyHex := "5840463aa6adcfd34213dd9738978bf3222d9e699924302b0d8b50adc25c7b9687c85054a05d94d266748f3b4233726938df87c3475036f72022efcfa46ff6a2d771"
	vrfKey, err := ParseVRFKeyCborHex(vrfKeyHex)
	if err != nil {
		t.Fatalf("VRF key parse failed: %v", err)
	}

	poolHex := "c825168836c5bf850dec38567eb4771c2e03eea28658ff291df768ae"
	poolBech32, err := convertToBech32(poolHex)
	if err != nil {
		t.Fatalf("Pool ID conversion: %v", err)
	}
	t.Logf("Pool bech32: %s", poolBech32)

	// Fetch epoch 467 nonce from Koios
	epochNonceHex, err := fetchKoiosEpochNonce(467)
	if err != nil {
		t.Fatalf("Koios nonce fetch: %v", err)
	}
	epochNonce, err := hex.DecodeString(epochNonceHex)
	if err != nil {
		t.Fatalf("Decode nonce: %v", err)
	}
	t.Logf("Epoch 467 nonce: %s", epochNonceHex)

	// Fetch pool stake for epoch 467 from Koios pool_history
	poolStake, totalStake, err := fetchKoiosStake(poolBech32, 467)
	if err != nil {
		t.Fatalf("Koios stake fetch: %v", err)
	}
	t.Logf("Pool stake: %d, Total stake: %d", poolStake, totalStake)
	t.Logf("Sigma: %.10f", float64(poolStake)/float64(totalStake))

	// Calculate schedule
	epochLength := GetEpochLength(MainnetNetworkMagic)
	epochStartSlot := GetEpochStartSlot(467, MainnetNetworkMagic)
	slotToTimeFn := makeSlotToTime(MainnetNetworkMagic)

	schedule, err := CalculateLeaderSchedule(
		467, epochNonce, vrfKey,
		poolStake, totalStake,
		epochLength, epochStartSlot, slotToTimeFn,
	)
	if err != nil {
		t.Fatalf("Schedule calculation failed: %v", err)
	}

	// Print results
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}

	fmt.Println("\n=== Epoch 467 Leader Schedule (OTG / Star Forge) ===")
	fmt.Printf("Epoch Nonce:     %s\n", epochNonceHex)
	fmt.Printf("Pool Stake:      %d lovelace (%.2f ADA)\n", poolStake, float64(poolStake)/1e6)
	fmt.Printf("Total Stake:     %d lovelace (%.2f B ADA)\n", totalStake, float64(totalStake)/1e15)
	fmt.Printf("Sigma:           %.10f\n", schedule.Sigma)
	fmt.Printf("Ideal Slots:     %.2f\n", schedule.IdealSlots)
	fmt.Printf("Assigned Slots:  %d\n\n", len(schedule.AssignedSlots))

	for _, s := range schedule.AssignedSlots {
		localTime := s.At.In(loc)
		fmt.Printf("  #%-3d Slot %-12d (epoch slot %-6d)  %s\n",
			s.No, s.Slot, s.SlotInEpoch, localTime.Format("2006-01-02 03:04:05 PM"))
	}
}

func fetchKoiosEpochNonce(epoch int) (string, error) {
	url := fmt.Sprintf("https://api.koios.rest/api/v1/epoch_params?_epoch_no=%d&select=nonce", epoch)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var result []struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if len(result) == 0 || result[0].Nonce == "" {
		return "", fmt.Errorf("no nonce for epoch %d", epoch)
	}
	return result[0].Nonce, nil
}

func fetchKoiosStake(poolBech32 string, epoch int) (uint64, uint64, error) {
	// Pool stake from pool_history
	url := fmt.Sprintf("https://api.koios.rest/api/v1/pool_history?_pool_bech32=%s&_epoch_no=%d", poolBech32, epoch)
	resp, err := http.Get(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var poolResult []struct {
		ActiveStake    json.Number `json:"active_stake"`
		ActiveStakeSum json.Number `json:"active_stake_pct"`
	}
	if err := json.Unmarshal(body, &poolResult); err != nil {
		return 0, 0, fmt.Errorf("pool history parse: %w", err)
	}
	if len(poolResult) == 0 {
		return 0, 0, fmt.Errorf("no pool history for epoch %d", epoch)
	}
	poolStake, err := poolResult[0].ActiveStake.Int64()
	if err != nil {
		return 0, 0, fmt.Errorf("pool stake parse: %w", err)
	}

	// Total stake from epoch_info
	url2 := fmt.Sprintf("https://api.koios.rest/api/v1/epoch_info?_epoch_no=%d&select=active_stake", epoch)
	resp2, err := http.Get(url2)
	if err != nil {
		return 0, 0, err
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 {
		return 0, 0, fmt.Errorf("HTTP %d: %s", resp2.StatusCode, string(body2))
	}
	var epochResult []struct {
		ActiveStake json.Number `json:"active_stake"`
	}
	if err := json.Unmarshal(body2, &epochResult); err != nil {
		return 0, 0, fmt.Errorf("epoch info parse: %w", err)
	}
	if len(epochResult) == 0 {
		return 0, 0, fmt.Errorf("no epoch info for epoch %d", epoch)
	}
	totalStake, err := epochResult[0].ActiveStake.Int64()
	if err != nil {
		return 0, 0, fmt.Errorf("total stake parse: %w", err)
	}

	return uint64(poolStake), uint64(totalStake), nil
}
