package main

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	koios "github.com/cardano-community/koios-go-client/v3"
	telebot "gopkg.in/tucnak/telebot.v2"
)

// TestComputeAndReplyLeaderlogRecomputesOnStaleStake is a regression test for
// #132: a cached schedule was served whenever its stored nonce matched, even
// if the stake used to compute it was stale/wrong (e.g. from the #131 bug).
// This stores a schedule with a nonce that matches the "current" nonce but a
// stake value that no longer matches what Koios reports, and asserts the
// stale schedule is NOT served as-is — a fresh one is computed and stored.
func TestComputeAndReplyLeaderlogRecomputesOnStaleStake(t *testing.T) {
	const targetEpoch = 500000
	currentNonce := make([]byte, 32)
	for i := range currentNonce {
		currentNonce[i] = byte(i + 1)
	}

	koiosSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pool_history":
			fmt.Fprintf(w, `[{"epoch_no":%d,"active_stake":"500","active_stake_pct":1.5}]`, targetEpoch)
		case "/api/v1/epoch_info":
			fmt.Fprintf(w, `[{"epoch_no":%d,"active_stake":"5000"}]`, targetEpoch)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer koiosSrv.Close()

	koiosClient, err := koios.New(koios.Host(koiosSrv.Listener.Addr().String()), koios.Scheme("http"), koios.APIVersion("v1"))
	if err != nil {
		t.Fatalf("failed to construct koios client: %v", err)
	}

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1}}}`)
	}))
	defer tgSrv.Close()

	bot, err := telebot.NewBot(telebot.Settings{URL: tgSrv.URL, Token: "test", Offline: true})
	if err != nil {
		t.Fatalf("failed to construct telebot bot: %v", err)
	}

	store := &stubStore{
		finalNonce: currentNonce,
		schedule: &LeaderSchedule{
			Epoch:      targetEpoch,
			EpochNonce: hex.EncodeToString(currentNonce),
			PoolStake:  100, // stale: no longer matches Koios (500)
			TotalStake: 1000,
		},
	}

	idx := &Indexer{
		koios:        koiosClient,
		bot:          bot,
		store:        store,
		networkMagic: PreviewNetworkMagic, // smaller epoch length keeps the VRF loop fast
		bech32PoolId: "pool1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		vrfKey:       &VRFKey{PrivateKey: make([]byte, 32), PublicKey: make([]byte, 32)},
		nonceTracker: &NonceTracker{store: store, networkMagic: PreviewNetworkMagic},
	}

	idx.computeAndReplyLeaderlog(&telebot.Chat{ID: 1}, targetEpoch)

	if store.insertedSchedule == nil {
		t.Fatalf("expected a freshly computed schedule to be stored, but stale cached schedule was served instead")
	}
	if store.insertedSchedule.PoolStake != 500 || store.insertedSchedule.TotalStake != 5000 {
		t.Errorf("expected recomputed schedule to use fresh Koios stake (pool=500 total=5000), got pool=%d total=%d",
			store.insertedSchedule.PoolStake, store.insertedSchedule.TotalStake)
	}
}

// TestComputeAndReplyLeaderlogServesCacheWhenStakeMatches asserts that a
// nonce-verified cached schedule whose stake still matches a fresh Koios
// lookup is served as-is, without triggering a recompute (no new schedule
// stored).
func TestComputeAndReplyLeaderlogServesCacheWhenStakeMatches(t *testing.T) {
	const targetEpoch = 500000
	currentNonce := make([]byte, 32)
	for i := range currentNonce {
		currentNonce[i] = byte(i + 1)
	}

	koiosSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pool_history":
			fmt.Fprintf(w, `[{"epoch_no":%d,"active_stake":"500","active_stake_pct":1.5}]`, targetEpoch)
		case "/api/v1/epoch_info":
			fmt.Fprintf(w, `[{"epoch_no":%d,"active_stake":"5000"}]`, targetEpoch)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer koiosSrv.Close()

	koiosClient, err := koios.New(koios.Host(koiosSrv.Listener.Addr().String()), koios.Scheme("http"), koios.APIVersion("v1"))
	if err != nil {
		t.Fatalf("failed to construct koios client: %v", err)
	}

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1}}}`)
	}))
	defer tgSrv.Close()

	bot, err := telebot.NewBot(telebot.Settings{URL: tgSrv.URL, Token: "test", Offline: true})
	if err != nil {
		t.Fatalf("failed to construct telebot bot: %v", err)
	}

	store := &stubStore{
		finalNonce: currentNonce,
		schedule: &LeaderSchedule{
			Epoch:      targetEpoch,
			EpochNonce: hex.EncodeToString(currentNonce),
			PoolStake:  500, // matches fresh Koios lookup
			TotalStake: 5000,
		},
	}

	idx := &Indexer{
		koios:        koiosClient,
		bot:          bot,
		store:        store,
		networkMagic: PreviewNetworkMagic,
		bech32PoolId: "pool1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		vrfKey:       &VRFKey{PrivateKey: make([]byte, 32), PublicKey: make([]byte, 32)},
		nonceTracker: &NonceTracker{store: store, networkMagic: PreviewNetworkMagic},
	}

	idx.computeAndReplyLeaderlog(&telebot.Chat{ID: 1}, targetEpoch)

	if store.insertedSchedule != nil {
		t.Fatalf("expected cached schedule to be served without recomputing, but a new schedule was stored: %+v",
			store.insertedSchedule)
	}
}

// TestComputeAndReplyLeaderlogServesCacheWhenStakeLookupFails asserts that a
// nonce-verified cached schedule is still served if the fresh stake
// re-verification lookup itself fails — a transient Koios error shouldn't
// force a full VRF recompute for an otherwise nonce-verified schedule.
func TestComputeAndReplyLeaderlogServesCacheWhenStakeLookupFails(t *testing.T) {
	const targetEpoch = 500000
	currentNonce := make([]byte, 32)
	for i := range currentNonce {
		currentNonce[i] = byte(i + 1)
	}

	koiosSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`) // no data for any epoch -> queryStakeForLeaderlog errors
	}))
	defer koiosSrv.Close()

	koiosClient, err := koios.New(koios.Host(koiosSrv.Listener.Addr().String()), koios.Scheme("http"), koios.APIVersion("v1"))
	if err != nil {
		t.Fatalf("failed to construct koios client: %v", err)
	}

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1}}}`)
	}))
	defer tgSrv.Close()

	bot, err := telebot.NewBot(telebot.Settings{URL: tgSrv.URL, Token: "test", Offline: true})
	if err != nil {
		t.Fatalf("failed to construct telebot bot: %v", err)
	}

	store := &stubStore{
		finalNonce: currentNonce,
		schedule: &LeaderSchedule{
			Epoch:      targetEpoch,
			EpochNonce: hex.EncodeToString(currentNonce),
			PoolStake:  500,
			TotalStake: 5000,
		},
	}

	idx := &Indexer{
		koios:        koiosClient,
		bot:          bot,
		store:        store,
		networkMagic: PreviewNetworkMagic,
		bech32PoolId: "pool1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		vrfKey:       &VRFKey{PrivateKey: make([]byte, 32), PublicKey: make([]byte, 32)},
		nonceTracker: &NonceTracker{store: store, networkMagic: PreviewNetworkMagic},
	}

	idx.computeAndReplyLeaderlog(&telebot.Chat{ID: 1}, targetEpoch)

	if store.insertedSchedule != nil {
		t.Fatalf("expected cached schedule to be served when stake lookup fails, but a new schedule was stored: %+v",
			store.insertedSchedule)
	}
}
