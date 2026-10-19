package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// koiosNonceServer serves epoch_params with nonceHex for every epoch, or an
// empty result when nonceHex is empty (Koios before it publishes the nonce).
func koiosNonceServer(t *testing.T, nonceHex string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if nonceHex == "" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"nonce":%q}]`, nonceHex)
	}))
	t.Cleanup(srv.Close)
	viper.Set("koios.url", srv.URL)
	t.Cleanup(func() { viper.Set("koios.url", "") })
}

func newVerifyTestIndexer(t *testing.T, epoch int, stored []byte) *Indexer {
	t.Helper()
	idx := newRollbackTestIndexer(t)
	idx.leaderlogCalcing = map[int]bool{}
	idx.leaderlogFailed = map[int]time.Time{}
	idx.scheduleExists = map[int]bool{epoch: true}
	if err := idx.store.SetFinalNonce(
		context.Background(), epoch, stored, "computed",
	); err != nil {
		t.Fatalf("SetFinalNonce: %v", err)
	}
	return idx
}

// A schedule posted from a locally computed next-epoch nonce must be checked
// against Koios once Koios publishes that nonce. On a mismatch Koios wins: the
// stored nonce is replaced and the schedule marked stale, so the next trigger
// recomputes and reposts it.
func TestVerifyScheduleNonceCorrectsMismatch(t *testing.T) {
	epoch := rollbackTestEpoch + 1
	wrong := bytes.Repeat([]byte{0xC1}, 32)
	right := bytes.Repeat([]byte{0x01}, 32)
	koiosNonceServer(t, hex.EncodeToString(right))
	idx := newVerifyTestIndexer(t, epoch, wrong)

	idx.verifyScheduleNonce(epoch)

	got, err := idx.store.GetFinalNonce(context.Background(), epoch)
	if err != nil {
		t.Fatalf("GetFinalNonce: %v", err)
	}
	if !bytes.Equal(got, right) {
		t.Fatalf("stored nonce = %x, want Koios nonce %x", got, right)
	}
	if idx.scheduleExists[epoch] {
		t.Fatal("schedule must be marked stale so it is recomputed and reposted")
	}
}

// A matching Koios nonce confirms the schedule; it is not checked again.
func TestVerifyScheduleNonceConfirmsMatch(t *testing.T) {
	epoch := rollbackTestEpoch + 1
	right := bytes.Repeat([]byte{0x01}, 32)
	koiosNonceServer(t, hex.EncodeToString(right))
	idx := newVerifyTestIndexer(t, epoch, right)

	idx.verifyScheduleNonce(epoch)

	if !idx.scheduleExists[epoch] || !idx.nonceVerified[epoch] {
		t.Fatalf("matching nonce must keep the schedule and mark it verified: exists=%v verified=%v",
			idx.scheduleExists[epoch], idx.nonceVerified[epoch])
	}
}

// Before Koios publishes the nonce nothing changes and the check repeats later.
func TestVerifyScheduleNonceWaitsForKoios(t *testing.T) {
	epoch := rollbackTestEpoch + 1
	stored := bytes.Repeat([]byte{0xC1}, 32)
	koiosNonceServer(t, "")
	idx := newVerifyTestIndexer(t, epoch, stored)

	idx.verifyScheduleNonce(epoch)

	got, err := idx.store.GetFinalNonce(context.Background(), epoch)
	if err != nil {
		t.Fatalf("GetFinalNonce: %v", err)
	}
	if !bytes.Equal(got, stored) || !idx.scheduleExists[epoch] || idx.nonceVerified[epoch] {
		t.Fatalf("unpublished Koios nonce must change nothing: nonce=%x exists=%v verified=%v",
			got, idx.scheduleExists[epoch], idx.nonceVerified[epoch])
	}
}
