package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestNonceVsKoios picks 25 random epochs from the database and compares
// our stored final_nonce against the Koios API epoch_params nonce.
// Requires GODUCKBOT_DB_PASSWORD env var and network access to Koios.
func TestNonceVsKoios(t *testing.T) {
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

	connURL := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("goduckbot", dbPass),
		Host:     fmt.Sprintf("%s:%s", dbHost, dbPort),
		Path:     "goduckbot_v2",
		RawQuery: "sslmode=disable",
	}
	store, err := NewPgStore(connURL.String())
	if err != nil {
		t.Fatalf("DB connect failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Get all epochs that have a final_nonce
	rows, err := store.pool.Query(ctx,
		"SELECT epoch, encode(final_nonce, 'hex') FROM epoch_nonces WHERE final_nonce IS NOT NULL ORDER BY epoch ASC")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	type epochNonce struct {
		epoch int
		nonce string
	}
	var allNonces []epochNonce
	for rows.Next() {
		var en epochNonce
		if err := rows.Scan(&en.epoch, &en.nonce); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		allNonces = append(allNonces, en)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("Row error: %v", err)
	}

	if len(allNonces) == 0 {
		t.Fatal("No epochs with final_nonce found in database")
	}
	t.Logf("Found %d epochs with final nonces (epoch %d to %d)", len(allNonces), allNonces[0].epoch, allNonces[len(allNonces)-1].epoch)

	// Pick 25 random epochs (or all if fewer than 25)
	sampleSize := 25
	if len(allNonces) < sampleSize {
		sampleSize = len(allNonces)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	perm := rng.Perm(len(allNonces))
	selected := make([]epochNonce, sampleSize)
	for i := 0; i < sampleSize; i++ {
		selected[i] = allNonces[perm[i]]
	}

	// Compare each against Koios
	passed := 0
	failed := 0
	errors := 0

	for i, en := range selected {
		koiosNonce, fetchErr := fetchKoiosNonce(en.epoch)
		if fetchErr != nil {
			t.Logf("[%2d/25] Epoch %d: KOIOS ERROR: %v", i+1, en.epoch, fetchErr)
			errors++
			continue
		}

		if en.nonce == koiosNonce {
			t.Logf("[%2d/25] Epoch %d: MATCH  %s", i+1, en.epoch, en.nonce[:16]+"...")
			passed++
		} else {
			t.Logf("[%2d/25] Epoch %d: MISMATCH", i+1, en.epoch)
			t.Logf("         Local: %s", en.nonce)
			t.Logf("         Koios: %s", koiosNonce)
			failed++
		}

		// Koios rate limit: ~100 req/s, but be polite
		time.Sleep(100 * time.Millisecond)
	}

	t.Logf("\n=== Results: %d passed, %d failed, %d errors (out of %d) ===", passed, failed, errors, sampleSize)
	if failed > 0 {
		t.Fatalf("%d/%d nonce mismatches detected", failed, sampleSize)
	}
}

func fetchKoiosNonce(epoch int) (string, error) {
	url := fmt.Sprintf("https://api.koios.rest/api/v1/epoch_params?_epoch_no=%d&select=nonce", epoch)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
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

	// Koios returns hex string, verify it decodes
	if _, err := hex.DecodeString(result[0].Nonce); err != nil {
		return "", fmt.Errorf("invalid hex nonce: %s", result[0].Nonce)
	}

	return result[0].Nonce, nil
}
