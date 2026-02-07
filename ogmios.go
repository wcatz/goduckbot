package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OgmiosClient provides an HTTP JSON-RPC client for querying Ogmios v6 instances.
type OgmiosClient struct {
	url        string
	httpClient *http.Client
}

// NewOgmiosClient creates a new Ogmios client.
// url should be like "http://ogmios-mainnet.cardano.svc.cluster.local:1337"
func NewOgmiosClient(url string) *OgmiosClient {
	return &OgmiosClient{
		url: url,
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
func (c *OgmiosClient) QueryTip(ctx context.Context) (slot uint64, blockHash string, epoch int, err error) {
	result, err := c.query(ctx, "queryNetwork/tip")
	if err != nil {
		return 0, "", 0, fmt.Errorf("queryNetwork/tip: %w", err)
	}

	var tipData struct {
		Slot  uint64 `json:"slot"`
		ID    string `json:"id"`
		Epoch int    `json:"epoch"`
	}
	if err := json.Unmarshal(result, &tipData); err != nil {
		return 0, "", 0, fmt.Errorf("parsing tip: %w", err)
	}
	return tipData.Slot, tipData.ID, tipData.Epoch, nil
}

// QueryStakeDistribution returns the live stake distribution (mark snapshot).
// Returns map of pool_id_bech32 -> stake_lovelace.
func (c *OgmiosClient) QueryStakeDistribution(ctx context.Context) (map[string]uint64, error) {
	result, err := c.query(ctx, "queryLedgerState/liveStakeDistribution")
	if err != nil {
		return nil, fmt.Errorf("queryLedgerState/liveStakeDistribution: %w", err)
	}

	// Ogmios v6 response: {"pool1abc...": {"ada": {"lovelace": N}, "lovelace": N}, ...}
	var rawDist map[string]struct {
		Lovelace uint64 `json:"lovelace"`
	}
	if err := json.Unmarshal(result, &rawDist); err != nil {
		return nil, fmt.Errorf("parsing stake distribution: %w", err)
	}

	dist := make(map[string]uint64, len(rawDist))
	for poolID, stake := range rawDist {
		dist[poolID] = stake.Lovelace
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
