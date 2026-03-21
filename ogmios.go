package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// OgmiosClient is a JSON-RPC 2.0 client for Ogmios v6.
type OgmiosClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
	id   atomic.Int64
}

type jsonRPCRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
	ID      int64       `json:"id"`
}

type jsonRPCResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonRPCError   `json:"error,omitempty"`
	ID      int64           `json:"id"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ProtocolParams holds the fee-relevant protocol parameters.
type ProtocolParams struct {
	MinFeeCoefficient uint64 `json:"minFeeCoefficient"`
	MinFeeConstant    struct {
		Ada struct {
			Lovelace uint64 `json:"lovelace"`
		} `json:"ada"`
	} `json:"minFeeConstant"`
}

// UTxO represents a single unspent transaction output.
type UTxO struct {
	TxHash  string
	Index   uint32
	Value   uint64
	Address string
}

func NewOgmiosClient(url string) (*OgmiosClient, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("connect ogmios: %w", err)
	}
	return &OgmiosClient{conn: conn}, nil
}

func (c *OgmiosClient) Close() error {
	return c.conn.Close()
}

func (c *OgmiosClient) call(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.id.Add(1)
	req := jsonRPCRequest{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	var resp jsonRPCResponse
	if err := c.conn.ReadJSON(&resp); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("ogmios error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	return resp.Result, nil
}

func (c *OgmiosClient) QueryProtocolParameters() (*ProtocolParams, error) {
	result, err := c.call("queryLedgerState/protocolParameters", nil)
	if err != nil {
		return nil, err
	}

	var params ProtocolParams
	if err := json.Unmarshal(result, &params); err != nil {
		return nil, fmt.Errorf("parse protocol params: %w", err)
	}

	return &params, nil
}

func (c *OgmiosClient) QueryUTxOs(address string) ([]UTxO, error) {
	params := map[string]interface{}{
		"addresses": []string{address},
	}

	result, err := c.call("queryLedgerState/utxo", params)
	if err != nil {
		return nil, err
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("parse utxo list: %w", err)
	}

	var utxos []UTxO
	for _, r := range raw {
		var obj struct {
			Transaction struct {
				ID string `json:"id"`
			} `json:"transaction"`
			Index   uint32 `json:"index"`
			Address string `json:"address"`
			Value   struct {
				Ada struct {
					Lovelace uint64 `json:"lovelace"`
				} `json:"ada"`
			} `json:"value"`
		}
		if err := json.Unmarshal(r, &obj); err != nil {
			return nil, fmt.Errorf("parse utxo: %w", err)
		}
		utxos = append(utxos, UTxO{
			TxHash:  obj.Transaction.ID,
			Index:   obj.Index,
			Value:   obj.Value.Ada.Lovelace,
			Address: obj.Address,
		})
	}

	return utxos, nil
}

func (c *OgmiosClient) QueryTip() (uint64, error) {
	result, err := c.call("queryLedgerState/tip", nil)
	if err != nil {
		return 0, err
	}

	var tip struct {
		Slot uint64 `json:"slot"`
	}
	if err := json.Unmarshal(result, &tip); err != nil {
		return 0, fmt.Errorf("parse tip: %w", err)
	}

	return tip.Slot, nil
}
