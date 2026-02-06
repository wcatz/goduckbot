package main

import (
	"testing"
)

func TestSlotToEpochMainnet(t *testing.T) {
	tests := []struct {
		slot  uint64
		epoch int
	}{
		{0, 0},                    // First Byron slot
		{21599, 0},                // Last slot of Byron epoch 0
		{21600, 1},                // First slot of Byron epoch 1
		{4492799, 207},            // Last Byron slot
		{4492800, 208},            // First Shelley slot
		{4492800 + 431999, 208},   // Last slot of Shelley epoch 208
		{4492800 + 432000, 209},   // First slot of epoch 209
		{4492800 + 432000*10, 218}, // 10 Shelley epochs in
	}

	for _, tt := range tests {
		got := SlotToEpoch(tt.slot, MainnetNetworkMagic)
		if got != tt.epoch {
			t.Errorf("SlotToEpoch(%d, mainnet) = %d, want %d", tt.slot, got, tt.epoch)
		}
	}
}

func TestSlotToEpochPreprod(t *testing.T) {
	tests := []struct {
		slot  uint64
		epoch int
	}{
		{0, 0},
		{21600, 1},
		{86399, 3},                  // Last Byron slot
		{86400, 4},                  // First Shelley slot
		{86400 + 431999, 4},         // Last slot of epoch 4
		{86400 + 432000, 5},         // First slot of epoch 5
	}

	for _, tt := range tests {
		got := SlotToEpoch(tt.slot, PreprodNetworkMagic)
		if got != tt.epoch {
			t.Errorf("SlotToEpoch(%d, preprod) = %d, want %d", tt.slot, got, tt.epoch)
		}
	}
}

func TestSlotToEpochPreview(t *testing.T) {
	tests := []struct {
		slot  uint64
		epoch int
	}{
		{0, 0},
		{86399, 0},
		{86400, 1},
		{172800, 2},
	}

	for _, tt := range tests {
		got := SlotToEpoch(tt.slot, PreviewNetworkMagic)
		if got != tt.epoch {
			t.Errorf("SlotToEpoch(%d, preview) = %d, want %d", tt.slot, got, tt.epoch)
		}
	}
}

func TestGetEpochStartSlotRoundTrip(t *testing.T) {
	// Verify SlotToEpoch(GetEpochStartSlot(e)) == e for various epochs/networks
	networks := []struct {
		name  string
		magic int
	}{
		{"mainnet", MainnetNetworkMagic},
		{"preprod", PreprodNetworkMagic},
		{"preview", PreviewNetworkMagic},
	}

	for _, net := range networks {
		for epoch := 0; epoch < 300; epoch++ {
			startSlot := GetEpochStartSlot(epoch, net.magic)
			gotEpoch := SlotToEpoch(startSlot, net.magic)
			if gotEpoch != epoch {
				t.Errorf("%s: SlotToEpoch(GetEpochStartSlot(%d)) = %d, want %d (startSlot=%d)",
					net.name, epoch, gotEpoch, epoch, startSlot)
			}
		}
	}
}

func TestGetEpochLength(t *testing.T) {
	if GetEpochLength(MainnetNetworkMagic) != MainnetEpochLength {
		t.Error("mainnet epoch length wrong")
	}
	if GetEpochLength(PreprodNetworkMagic) != MainnetEpochLength {
		t.Error("preprod epoch length wrong")
	}
	if GetEpochLength(PreviewNetworkMagic) != PreviewEpochLength {
		t.Error("preview epoch length wrong")
	}
}

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-1234567, "-1,234,567"},
		{1000000000, "1,000,000,000"},
	}

	for _, tt := range tests {
		got := formatNumber(tt.n)
		if got != tt.want {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
