package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	koios "github.com/cardano-community/koios-go-client/v3"
)

// TestQueryStakeForLeaderlogRejectsWrongEpoch is a regression test for #131:
// the Koios fallback previously returned whatever epoch it found data for
// first (targetEpoch, curEpoch, curEpoch-1, curEpoch-2), silently substituting
// a different epoch's active stake for the target epoch. Active stake is
// epoch-specific, so that substitution produces incorrect leader schedules.
// This test serves data only for a "wrong" epoch and asserts the function
// refuses it rather than accepting it as a stand-in for targetEpoch.
func TestQueryStakeForLeaderlogRejectsWrongEpoch(t *testing.T) {
	curEpoch := calcCurrentEpoch(MainnetNetworkMagic)
	const targetEpoch = 999999 // far in the future: never has real data
	wrongEpoch := curEpoch     // old buggy code fell back to this and returned its stake

	requestedEpochs := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		requestedEpochs[r.URL.Path]++
		epochNo := q.Get("_epoch_no")

		w.Header().Set("Content-Type", "application/json")
		if epochNo != fmt.Sprint(wrongEpoch) {
			fmt.Fprint(w, `[]`)
			return
		}

		switch r.URL.Path {
		case "/api/v1/pool_history":
			fmt.Fprintf(w, `[{"epoch_no":%d,"active_stake":"123456789","active_stake_pct":1.5}]`, wrongEpoch)
		case "/api/v1/epoch_info":
			fmt.Fprintf(w, `[{"epoch_no":%d,"active_stake":"9999999999"}]`, wrongEpoch)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	host := srv.Listener.Addr().String()
	client, err := koios.New(koios.Host(host), koios.Scheme("http"), koios.APIVersion("v1"))
	if err != nil {
		t.Fatalf("failed to construct koios client: %v", err)
	}

	idx := &Indexer{
		koios:        client,
		networkMagic: MainnetNetworkMagic,
		bech32PoolId: "pool1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
	}

	_, _, err = idx.queryStakeForLeaderlog(context.Background(), targetEpoch)
	if err == nil {
		t.Fatalf("expected error when only a different epoch (%d) has data for target epoch %d, got success", wrongEpoch, targetEpoch)
	}

	if n := requestedEpochs["/api/v1/pool_history"]; n != 1 {
		t.Errorf("expected exactly 1 request to /pool_history (no cross-epoch fallback loop), got %d", n)
	}
}
