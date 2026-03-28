package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MessageTraceResult represents a single message trace entry from the Graph API.
type MessageTraceResult struct {
	RecipientAddress string `json:"recipientAddress"`
	SenderAddress    string `json:"senderAddress"`
	Status           string `json:"status"`
	ReceivedDateTime string `json:"receivedDateTime"`
	InternetMessageID string `json:"internetMessageId"`
	Subject          string `json:"subject"`
}

type messageTraceResponse struct {
	Value    []MessageTraceResult `json:"value"`
	NextLink string               `json:"@odata.nextLink"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// MessageTrace queries the Graph message trace API for a sender within a time range.
// Returns all trace entries found. Requires app-only auth with ExchangeMessageTrace.Read.All.
func (c *Client) MessageTrace(ctx context.Context, senderAddress string, receivedTime time.Time) ([]MessageTraceResult, error) {
	if c.certTokenProvider == nil {
		return nil, fmt.Errorf("message trace requires cert_path in graph config")
	}

	token, err := c.certTokenProvider.getToken()
	if err != nil {
		return nil, fmt.Errorf("get app-only token: %w", err)
	}

	// Query ±1 day around the received time
	start := receivedTime.Add(-24 * time.Hour).UTC().Truncate(24 * time.Hour)
	end := receivedTime.Add(24 * time.Hour).UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)

	filter := fmt.Sprintf(
		"senderAddress eq '%s' and receivedDateTime ge %s and receivedDateTime le %s",
		senderAddress,
		start.Format("2006-01-02T15:04:05Z"),
		end.Format("2006-01-02T15:04:05Z"),
	)

	params := url.Values{}
	params.Set("$filter", filter)
	params.Set("$top", "20")

	endpoint := "https://graph.microsoft.com/beta/admin/exchange/tracing/messageTraces?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("message trace request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read message trace response: %w", err)
	}

	var result messageTraceResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse message trace response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("message trace API error: %s: %s", result.Error.Code, result.Error.Message)
	}

	return result.Value, nil
}

// LookupEnvelopeRecipient finds the original envelope recipient for a message
// by querying the Graph message trace API. Returns the original RCPT TO address
// (before alias resolution), or empty string if not found.
//
// The trace API returns two entries for alias-resolved messages:
//   - status="Resolved" → original envelope recipient (the alias)
//   - status="Delivered" → final mailbox address (primary SMTP)
//
// We prefer the "Resolved" entry since that's the original address.
func (c *Client) LookupEnvelopeRecipient(ctx context.Context, senderAddress string, receivedTime time.Time) (string, error) {
	traces, err := c.MessageTrace(ctx, senderAddress, receivedTime)
	if err != nil {
		return "", err
	}

	var fallback string
	for _, t := range traces {
		addr := strings.TrimSpace(t.RecipientAddress)
		if addr == "" {
			continue
		}
		// "Resolved" = original envelope recipient (before alias resolution)
		if strings.EqualFold(t.Status, "resolved") {
			return addr, nil
		}
		if fallback == "" {
			fallback = addr
		}
	}

	return fallback, nil
}
