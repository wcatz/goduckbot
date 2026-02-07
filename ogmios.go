package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// OgmiosClient provides an HTTP JSON-RPC client for querying Ogmios v6 instances.
type OgmiosClient struct {
	url          string
	networkMagic int
	httpClient   *http.Client
}

// NewOgmiosClient creates a new Ogmios client.
// url should be like "http://ogmios-mainnet.cardano.svc.cluster.local:1337"
func NewOgmiosClient(url string, networkMagic int) *OgmiosClient {
	return &OgmiosClient{
		url:          url,
		networkMagic: networkMagic,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *OgmiosClient) Close() error {
	return nil
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	ID      interface{} `json:"id"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *OgmiosClient) query(ctx context.Context, method string) (json.RawMessage, error) {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		ID:      nil,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshaling response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

// QueryTip returns the current chain tip.
// Ogmios v6 does not return epoch in the tip response, so we derive it from slot.
func (c *OgmiosClient) QueryTip(ctx context.Context) (slot uint64, blockHash string, epoch int, err error) {
	result, err := c.query(ctx, "queryNetwork/tip")
	if err != nil {
		return 0, "", 0, fmt.Errorf("queryNetwork/tip: %w", err)
	}

	var tipData struct {
		Slot uint64 `json:"slot"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(result, &tipData); err != nil {
		return 0, "", 0, fmt.Errorf("parsing tip: %w", err)
	}
	derivedEpoch := SlotToEpoch(tipData.Slot, c.networkMagic)
	return tipData.Slot, tipData.ID, derivedEpoch, nil
}

// QueryStakeDistribution returns the live stake distribution (mark snapshot).
// Returns map of pool_id_bech32 -> relative_stake (scaled to preserve ratios).
// Ogmios v6 returns stake as rational fractions ("N/D"), not absolute lovelace.
func (c *OgmiosClient) QueryStakeDistribution(ctx context.Context) (map[string]uint64, error) {
	result, err := c.query(ctx, "queryLedgerState/liveStakeDistribution")
	if err != nil {
		return nil, fmt.Errorf("queryLedgerState/liveStakeDistribution: %w", err)
	}

	// Ogmios v6 response: {"pool1abc...": {"stake": "N/D", "vrf": "hex"}, ...}
	var rawDist map[string]struct {
		Stake string `json:"stake"`
	}
	if err := json.Unmarshal(result, &rawDist); err != nil {
		return nil, fmt.Errorf("parsing stake distribution: %w", err)
	}

	// Parse rational fractions and scale to pseudo-lovelace values.
	// Each "stake" is a fraction representing sigma (relative stake).
	// We multiply by 1e15 to preserve precision as uint64.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(15), nil)
	dist := make(map[string]uint64, len(rawDist))
	for poolID, entry := range rawDist {
		parts := strings.SplitN(entry.Stake, "/", 2)
		if len(parts) != 2 {
			continue
		}
		num, ok1 := new(big.Int).SetString(parts[0], 10)
		den, ok2 := new(big.Int).SetString(parts[1], 10)
		if !ok1 || !ok2 || den.Sign() == 0 {
			continue
		}
		// scaled = num * 1e15 / den
		scaled := new(big.Int).Mul(num, scale)
		scaled.Div(scaled, den)
		dist[poolID] = scaled.Uint64()
	}
	return dist, nil
}

// QueryEpochNonce returns the current epoch nonce from the ledger state.
func (c *OgmiosClient) QueryEpochNonce(ctx context.Context) ([]byte, error) {
	result, err := c.query(ctx, "queryLedgerState/nonces")
	if err != nil {
		return nil, fmt.Errorf("queryLedgerState/nonces: %w", err)
	}

	var noncesData struct {
		Current string `json:"current"`
	}
	if err := json.Unmarshal(result, &noncesData); err != nil {
		return nil, fmt.Errorf("parsing nonces: %w", err)
	}

	nonceBytes, err := hex.DecodeString(noncesData.Current)
	if err != nil {
		return nil, fmt.Errorf("decoding nonce hex: %w", err)
	}
	return nonceBytes, nil
}

// QueryProtocolParameters returns the raw protocol parameters JSON.
func (c *OgmiosClient) QueryProtocolParameters(ctx context.Context) (json.RawMessage, error) {
	result, err := c.query(ctx, "queryLedgerState/protocolParameters")
	if err != nil {
		return nil, fmt.Errorf("queryLedgerState/protocolParameters: %w", err)
	}
	return result, nil
}
