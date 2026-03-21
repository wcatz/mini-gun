package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

// SubmitTx posts a CBOR-serialized transaction to tx-submit-api.
func SubmitTx(submitURL string, cborBytes []byte) error {
	resp, err := httpClient.Post(
		submitURL+"/api/submit/tx",
		"application/cbor",
		bytes.NewReader(cborBytes),
	)
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
