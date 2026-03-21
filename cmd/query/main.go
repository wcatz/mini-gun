package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gorilla/websocket"
)

func main() {
	url := "ws://localhost:1337"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	addrs := []string{
		"addr_test1vrj0l2t0glhe3ss24azr2njpv9n43ucwfdauzufa0p3ru8c98svrj",
		"addr_test1vrdtcvrpdzefag83r2820wgeuary0zm78p0k644hemqreycgk4k8t",
	}

	for i, addr := range addrs {
		req := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "queryLedgerState/utxo",
			"params":  map[string]interface{}{"addresses": []string{addr}},
			"id":      i + 1,
		}
		if err := conn.WriteJSON(req); err != nil {
			fmt.Fprintf(os.Stderr, "write: %v\n", err)
			os.Exit(1)
		}
		var resp json.RawMessage
		if err := conn.ReadJSON(&resp); err != nil {
			fmt.Fprintf(os.Stderr, "read: %v\n", err)
			os.Exit(1)
		}

		var parsed struct {
			Result []struct {
				Transaction struct {
					ID string `json:"id"`
				} `json:"transaction"`
				Index uint32 `json:"index"`
				Value struct {
					Ada struct {
						Lovelace uint64 `json:"lovelace"`
					} `json:"ada"`
				} `json:"value"`
			} `json:"result"`
		}
		json.Unmarshal(resp, &parsed)

		label := string(rune('A' + i))
		var total uint64
		fmt.Printf("Wallet %s: %s\n", label, addr[:20]+"...")
		for _, u := range parsed.Result {
			fmt.Printf("  %s#%d  %.6f ADA\n", u.Transaction.ID[:12], u.Index, float64(u.Value.Ada.Lovelace)/1_000_000)
			total += u.Value.Ada.Lovelace
		}
		fmt.Printf("  Total: %.6f ADA (%d UTXOs)\n\n", float64(total)/1_000_000, len(parsed.Result))
	}
}
