package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func sampleMint() mintNotification {
	return mintNotification{
		PoolName:       "Star Forge",
		TxCount:        5,
		SizePercentage: 3.65,
		Interval:       "22s",
		EpochBlocks:    9,
		TotalBlocks:    5384,
		HasBlock:       true,
		ScriptTxs:      1,
		Steps:          190_000_000,
		Mem:            500_000,
		MaxSteps:       20_000_000_000,
		MaxMem:         72_000_000,
		HasProto:       true,
		ProtoMinor:     69,
	}
}

// xWeightedLen approximates X's character count: URLs count as 23 and
// characters outside Latin ranges (emoji etc.) count as 2.
func xWeightedLen(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "https://") {
			n += 23 + 1
			continue
		}
		for _, r := range line {
			if r > 0x10FF {
				n += 2
			} else {
				n++
			}
		}
		n++
	}
	return n - 1
}

func TestFormatMintMessageTweetFits(t *testing.T) {
	n := sampleMint()
	n.PoolName = ""
	n.TxCount = 300
	n.ScriptTxs = 300
	n.SizePercentage = 100
	n.Interval = "120m59s"
	n.EpochBlocks = 999
	n.TotalBlocks = 9_999_999
	n.HasNodeBlocks = true
	n.NodeBlocks = 9_999_999
	n.Links = []string{adastatBlockURL(MainnetNetworkMagic, strings.Repeat("f", 64))}
	msg := formatMintMessage(n)
	if l := xWeightedLen(msg); l > 280 {
		t.Fatalf("worst-case tweet is %d weighted chars (> 280):\n%s", l, msg)
	}
}

func TestFormatMintMessageContent(t *testing.T) {
	n := sampleMint()
	msg := formatMintMessage(n)
	for _, want := range []string{"Star Forge", "Scripts: 1/5 txs", "Steps 0.9% · Mem 0.7%", "Node: Dingo"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}

	n.ScriptTxs, n.Steps, n.Mem = 0, 0, 0
	n.ProtoMinor = 0
	msg = formatMintMessage(n)
	if strings.Contains(msg, "Steps") || !strings.Contains(msg, "Node: Haskell") {
		t.Errorf("unexpected no-script/Haskell message:\n%s", msg)
	}

	n.HasBlock, n.HasProto = false, false
	msg = formatMintMessage(n)
	if strings.Contains(msg, "Scripts") || strings.Contains(msg, "Node:") {
		t.Errorf("block-less message should omit scripts and node:\n%s", msg)
	}
	if !utf8.ValidString(msg) {
		t.Error("invalid UTF-8")
	}
}

func TestFormatMintMessageNodeBlocks(t *testing.T) {
	n := sampleMint()
	n.HasNodeBlocks = true
	n.NodeBlocks = 43
	if msg := formatMintMessage(n); !strings.Contains(msg, "Node: Dingo(43)\n") {
		t.Errorf("missing Dingo count in:\n%s", msg)
	}
	n.ProtoMinor = 0
	n.NodeBlocks = 5348
	if msg := formatMintMessage(n); !strings.Contains(msg, "Node: Haskell(5348)\n") {
		t.Errorf("missing Haskell count in:\n%s", msg)
	}
}

func TestNodeBlockCount(t *testing.T) {
	if got := nodeBlockCount(dingoProtoMinor, 5391, 43); got != 43 {
		t.Errorf("dingo: got %d, want 43", got)
	}
	if got := nodeBlockCount(0, 5391, 43); got != 5348 {
		t.Errorf("haskell: got %d, want 5348", got)
	}
	if got := nodeBlockCount(0, 10, 43); got != 0 {
		t.Errorf("haskell underflow: got %d, want 0", got)
	}
}

func TestFetchPoolBlockCountByProtoMinor(t *testing.T) {
	var gotQuery, gotPrefer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotPrefer = r.URL.RawQuery, r.Header.Get("Prefer")
		w.Header().Set("Content-Range", "0-0/43")
		w.Write([]byte(`[{"block_height":1}]`))
	}))
	defer srv.Close()

	n, err := fetchPoolBlockCountByProtoMinor(context.Background(), srv.URL, "pool1abc", dingoProtoMinor)
	if err != nil || n != 43 {
		t.Fatalf("got %d, %v; want 43", n, err)
	}
	if gotPrefer != "count=exact" {
		t.Errorf("Prefer header = %q", gotPrefer)
	}
	for _, want := range []string{"pool=eq.pool1abc", "proto_minor=eq.69"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "*/0")
		w.Write([]byte(`[]`))
	}))
	defer empty.Close()
	if n, err := fetchPoolBlockCountByProtoMinor(context.Background(), empty.URL, "pool1abc", dingoProtoMinor); err != nil || n != 0 {
		t.Errorf("empty: got %d, %v; want 0", n, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer bad.Close()
	if _, err := fetchPoolBlockCountByProtoMinor(context.Background(), bad.URL, "pool1abc", dingoProtoMinor); err == nil {
		t.Error("want error on 429")
	}
}

func TestAdastatBlockURL(t *testing.T) {
	h := strings.Repeat("a", 64)
	cases := map[int]string{
		MainnetNetworkMagic: "https://adastat.net/blocks/" + h,
		PreprodNetworkMagic: "https://preprod.adastat.net/blocks/" + h,
		PreviewNetworkMagic: "https://preview.adastat.net/blocks/" + h,
	}
	for magic, want := range cases {
		if got := adastatBlockURL(magic, h); got != want {
			t.Errorf("magic %d: got %q, want %q", magic, got, want)
		}
	}
}

func TestNodeImplementation(t *testing.T) {
	if nodeImplementation(69) != "Dingo" || nodeImplementation(0) != "Haskell" {
		t.Error("wrong node implementation mapping")
	}
}
