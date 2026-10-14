package main

import (
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

func TestPooltoolBlockURL(t *testing.T) {
	if got := pooltoolBlockURL(MainnetNetworkMagic, 14065812); got != "https://pooltool.io/realtime/14065812" {
		t.Errorf("mainnet: got %q", got)
	}
	if got := pooltoolBlockURL(PreprodNetworkMagic, 1); got != "" {
		t.Errorf("preprod: got %q, want empty", got)
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
