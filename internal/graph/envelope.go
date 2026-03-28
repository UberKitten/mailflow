package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

type EnvelopeLookupConfig struct {
	Script              string `yaml:"script"`
	URL                 string `yaml:"url"`
	Timeout             int    `yaml:"timeout"`
	EnabledInProcessing bool   `yaml:"enabled_in_processing"`
}

type envelopeLookupResponse struct {
	RecipientAddress string `json:"recipientAddress"`
}

func LookupEnvelopeRecipient(ctx context.Context, cfg EnvelopeLookupConfig, messageID string, senderAddress string, receivedTime time.Time) (string, error) {
	if cfg.Script == "" && cfg.URL == "" {
		return "", nil
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	received := receivedTime.Format(time.RFC3339)

	if cfg.Script != "" {
		cmd := exec.CommandContext(ctx, cfg.Script, messageID, senderAddress, received)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			stderrText := strings.TrimSpace(stderr.String())
			if stderrText != "" {
				return "", fmt.Errorf("envelope lookup script failed: %w: %s", err, stderrText)
			}
			return "", fmt.Errorf("envelope lookup script failed: %w", err)
		}
		return strings.TrimSpace(stdout.String()), nil
	}

	reqURL, err := url.Parse(cfg.URL)
	if err != nil {
		return "", fmt.Errorf("invalid envelope lookup url: %w", err)
	}
	query := reqURL.Query()
	query.Set("messageId", messageID)
	query.Set("sender", senderAddress)
	query.Set("received", received)
	reqURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("envelope lookup http failed: %s - %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload envelopeLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode envelope lookup response: %w", err)
	}

	return strings.TrimSpace(payload.RecipientAddress), nil
}
