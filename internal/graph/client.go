package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mailflow/internal/config"

	"golang.org/x/sync/errgroup"
)

// ErrMessageGone is returned when a message was deleted or moved before we could act on it.
var ErrMessageGone = errors.New("message no longer exists")

type Client struct {
	baseURL     string
	tokenScript string
	httpClient  *http.Client

	sema                 chan struct{}
	maxConcurrent        int
	rangeWorkers         int
	largeFolderThreshold int
	rangeDays            int
	rng                  *rand.Rand
	rngMu                sync.Mutex
}

func NewClient(cfg *config.Config) (*Client, error) {
	c := &Client{
		baseURL:              cfg.Graph.BaseURL,
		tokenScript:          cfg.Graph.TokenScript,
		httpClient:           &http.Client{Timeout: 120 * time.Second},
		maxConcurrent:        cfg.Graph.MaxConcurrentRequests,
		rangeWorkers:         cfg.Graph.RangeWorkers,
		largeFolderThreshold: cfg.Graph.LargeFolderThreshold,
		rangeDays:            cfg.Graph.RangeDays,
		rng:                  rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if c.maxConcurrent > 0 {
		c.sema = make(chan struct{}, c.maxConcurrent)
	}
	return c, nil
}

type Message struct {
	ID       string
	Subject  string
	From     string
	FromName string
	To       []string
	Body     string
	BodyHTML string
	Snippet  string
	IsRead   bool
	Received time.Time
}

type graphMessage struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	From    struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
	ToRecipients []struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"toRecipients"`
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	BodyPreview      string `json:"bodyPreview"`
	IsRead           bool   `json:"isRead"`
	ReceivedDateTime string `json:"receivedDateTime"`
}

type listResponse struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

func (c *Client) token() (string, error) {
	cmd := exec.Command(filepath.Clean(c.tokenScript))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token script failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// GetToken returns a valid access token (public wrapper for subscription manager)
func (c *Client) GetToken() (string, error) {
	return c.token()
}

func (c *Client) do(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	if c.sema != nil {
		select {
		case c.sema <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-c.sema }()
	}

	token, err := c.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return c.httpClient.Do(req)
}

func (c *Client) doWithRetry(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	const maxRetries = 5
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, method, url, body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		if attempt >= maxRetries {
			resp.Body.Close()
			return nil, fmt.Errorf("request failed after retries: %s", resp.Status)
		}

		delay := time.Second * time.Duration(1<<attempt)
		if resp.StatusCode == http.StatusTooManyRequests {
			if retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				delay = retryAfter
			}
		}
		delay = c.jitter(delay)
		slog.Warn("graph request retry", "status", resp.Status, "attempt", attempt+1, "delay", delay, "url", url)
		resp.Body.Close()

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func parseRetryAfter(val string) (time.Duration, bool) {
	if val == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(val); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(val); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func (c *Client) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	c.rngMu.Lock()
	offset := (c.rng.Float64()*0.5 - 0.25) * float64(d)
	c.rngMu.Unlock()
	return d + time.Duration(offset)
}

func (c *Client) FindFolderIDByPath(ctx context.Context, path string) (string, error) {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid path")
	}

	// Start at top-level
	parentID := ""
	for _, part := range parts {
		id, err := c.findChildFolder(ctx, parentID, part)
		if err != nil {
			return "", err
		}
		parentID = id
	}
	return parentID, nil
}

func (c *Client) findChildFolder(ctx context.Context, parentID, name string) (string, error) {
	var endpoint string
	if parentID == "" {
		endpoint = c.baseURL + "/me/mailFolders"
	} else {
		endpoint = fmt.Sprintf("%s/me/mailFolders/%s/childFolders", c.baseURL, parentID)
	}

	reqURL := endpoint + "?$top=200"
	for {
		resp, err := c.doWithRetry(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("list folders failed: %s", resp.Status)
		}
		var lr struct {
			Value []struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayName"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
			return "", err
		}
		for _, f := range lr.Value {
			if f.DisplayName == name {
				return f.ID, nil
			}
		}
		if lr.NextLink == "" {
			break
		}
		reqURL = lr.NextLink
	}

	return "", fmt.Errorf("folder not found: %s", name)
}

// ListFolderTree returns folder IDs with path for recursive scans.
type FolderInfo struct {
	ID   string
	Path string
}

func (c *Client) ListFolderTree(ctx context.Context, rootID, rootPath string) ([]FolderInfo, error) {
	var out []FolderInfo
	queue := []FolderInfo{{ID: rootID, Path: rootPath}}
	out = append(out, FolderInfo{ID: rootID, Path: rootPath})

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		children, err := c.listChildFolders(ctx, cur.ID)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			path := child.DisplayName
			if cur.Path != "" {
				path = cur.Path + "/" + child.DisplayName
			}
			out = append(out, FolderInfo{ID: child.ID, Path: path})
			queue = append(queue, FolderInfo{ID: child.ID, Path: path})
		}
	}

	return out, nil
}

type folderSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

func (c *Client) listChildFolders(ctx context.Context, parentID string) ([]folderSummary, error) {
	endpoint := fmt.Sprintf("%s/me/mailFolders/%s/childFolders?$top=200", c.baseURL, parentID)
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list child folders failed: %s", resp.Status)
	}
	var lr struct {
		Value []folderSummary `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, err
	}
	return lr.Value, nil
}

// ListOptions controls message listing.
type ListOptions struct {
	OnlyUnread bool
	Since      time.Duration
	Fields     []string
	Fast       bool
}

func (c *Client) ListMessages(ctx context.Context, folderID string, opts ListOptions) ([]Message, error) {
	var out []Message
	if err := c.StreamMessages(ctx, folderID, opts, func(msg Message) error {
		out = append(out, msg)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

type dateRange struct {
	Start time.Time
	End   time.Time
}

// StreamMessages streams messages from Graph in pages and invokes fn for each message.
// If fn returns an error, streaming stops and that error is returned.
func (c *Client) StreamMessages(ctx context.Context, folderID string, opts ListOptions, fn func(msg Message) error) error {
	params := url.Values{}
	params.Set("$top", "50")
	params.Set("$orderby", "receivedDateTime desc")
	params.Set("$select", buildSelect(opts))

	var filtersNoDate []string
	if opts.OnlyUnread {
		filtersNoDate = append(filtersNoDate, "isRead eq false")
	}

	var sinceTime time.Time
	if opts.Since > 0 {
		sinceTime = time.Now().Add(-opts.Since).UTC()
	}

	if c.shouldBatch(ctx, folderID) {
		start, end, ok, err := c.getDateBounds(ctx, folderID, filtersNoDate, sinceTime)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		ranges := buildDateRanges(start, end, c.rangeDays)
		results := make([][]Message, len(ranges))

		g, gctx := errgroup.WithContext(ctx)
		sem := make(chan struct{}, c.rangeWorkers)
		for i, r := range ranges {
			i := i
			r := r
			g.Go(func() error {
				sem <- struct{}{}
				defer func() { <-sem }()
				msgs, err := c.listMessagesRange(gctx, folderID, params, filtersNoDate, r.Start, r.End)
				if err != nil {
					return err
				}
				results[i] = msgs
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		for _, msgs := range results {
			for _, msg := range msgs {
				if err := fn(msg); err != nil {
					return err
				}
			}
		}
		return nil
	}

	filters := append([]string{}, filtersNoDate...)
	if !sinceTime.IsZero() {
		filters = append(filters, fmt.Sprintf("receivedDateTime ge %s", sinceTime.Format(time.RFC3339)))
	}
	if len(filters) > 0 {
		params.Set("$filter", strings.Join(filters, " and "))
	}

	endpoint := fmt.Sprintf("%s/me/mailFolders/%s/messages?%s", c.baseURL, folderID, params.Encode())
	return c.streamMessagesEndpoint(ctx, endpoint, func(m graphMessage) error {
		return fn(toMessage(m))
	})
}

func buildSelect(opts ListOptions) string {
	var fields []string
	if len(opts.Fields) > 0 {
		fields = append(fields, opts.Fields...)
	} else if opts.Fast {
		fields = []string{"id", "from", "subject", "toRecipients", "receivedDateTime"}
	} else {
		fields = []string{"id", "subject", "from", "toRecipients", "body", "bodyPreview", "isRead", "receivedDateTime"}
	}
	if !containsField(fields, "receivedDateTime") {
		fields = append(fields, "receivedDateTime")
	}
	return strings.Join(fields, ",")
}

func containsField(fields []string, field string) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

func (c *Client) shouldBatch(ctx context.Context, folderID string) bool {
	if c.largeFolderThreshold <= 0 || c.rangeDays <= 0 || c.rangeWorkers <= 1 {
		return false
	}
	count, err := c.getFolderTotalCount(ctx, folderID)
	if err != nil {
		slog.Warn("folder count unavailable", "folder", folderID, "error", err)
		return false
	}
	return count >= c.largeFolderThreshold
}

func (c *Client) getFolderTotalCount(ctx context.Context, folderID string) (int, error) {
	endpoint := fmt.Sprintf("%s/me/mailFolders/%s?$select=totalItemCount", c.baseURL, folderID)
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("folder count failed: %s", resp.Status)
	}
	var data struct {
		TotalItemCount int `json:"totalItemCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, err
	}
	return data.TotalItemCount, nil
}

func (c *Client) getDateBounds(ctx context.Context, folderID string, filters []string, since time.Time) (time.Time, time.Time, bool, error) {
	var start time.Time
	if since.IsZero() {
		earliest, ok, err := c.getEdgeDate(ctx, folderID, filters, true)
		if err != nil {
			return time.Time{}, time.Time{}, false, err
		}
		if !ok {
			return time.Time{}, time.Time{}, false, nil
		}
		start = earliest
	} else {
		start = since
	}

	latest, ok, err := c.getEdgeDate(ctx, folderID, filters, false)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if !ok {
		return time.Time{}, time.Time{}, false, nil
	}
	if latest.Before(start) {
		return time.Time{}, time.Time{}, false, nil
	}
	endExclusive := latest.Add(time.Second)
	return start, endExclusive, true, nil
}

func (c *Client) getEdgeDate(ctx context.Context, folderID string, filters []string, earliest bool) (time.Time, bool, error) {
	params := url.Values{}
	params.Set("$top", "1")
	if earliest {
		params.Set("$orderby", "receivedDateTime")
	} else {
		params.Set("$orderby", "receivedDateTime desc")
	}
	params.Set("$select", "receivedDateTime")
	if len(filters) > 0 {
		params.Set("$filter", strings.Join(filters, " and "))
	}
	endpoint := fmt.Sprintf("%s/me/mailFolders/%s/messages?%s", c.baseURL, folderID, params.Encode())
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return time.Time{}, false, fmt.Errorf("edge query failed: %s", resp.Status)
	}
	var lr listResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return time.Time{}, false, err
	}
	if len(lr.Value) == 0 {
		return time.Time{}, false, nil
	}
	when, err := time.Parse(time.RFC3339, lr.Value[0].ReceivedDateTime)
	if err != nil {
		return time.Time{}, false, err
	}
	return when, true, nil
}

func buildDateRanges(start, end time.Time, days int) []dateRange {
	if days <= 0 {
		return []dateRange{{Start: start, End: end}}
	}
	var ranges []dateRange
	step := time.Duration(days) * 24 * time.Hour
	cur := start
	for cur.Before(end) {
		next := cur.Add(step)
		if next.After(end) {
			next = end
		}
		ranges = append(ranges, dateRange{Start: cur, End: next})
		cur = next
	}
	// Reverse to process newest ranges first
	for i, j := 0, len(ranges)-1; i < j; i, j = i+1, j-1 {
		ranges[i], ranges[j] = ranges[j], ranges[i]
	}
	return ranges
}

func (c *Client) listMessagesRange(ctx context.Context, folderID string, baseParams url.Values, filters []string, start, end time.Time) ([]Message, error) {
	params := url.Values{}
	for k, v := range baseParams {
		params[k] = append([]string{}, v...)
	}
	filters = append([]string{}, filters...)
	if !start.IsZero() {
		filters = append(filters, fmt.Sprintf("receivedDateTime ge %s", start.Format(time.RFC3339)))
	}
	if !end.IsZero() {
		filters = append(filters, fmt.Sprintf("receivedDateTime lt %s", end.Format(time.RFC3339)))
	}
	if len(filters) > 0 {
		params.Set("$filter", strings.Join(filters, " and "))
	}
	endpoint := fmt.Sprintf("%s/me/mailFolders/%s/messages?%s", c.baseURL, folderID, params.Encode())
	var msgs []Message
	if err := c.streamMessagesEndpoint(ctx, endpoint, func(m graphMessage) error {
		msgs = append(msgs, toMessage(m))
		return nil
	}); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (c *Client) streamMessagesEndpoint(ctx context.Context, endpoint string, fn func(graphMessage) error) error {
	for {
		resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			return fmt.Errorf("list messages failed: %s", resp.Status)
		}
		var lr listResponse
		if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()
		for _, m := range lr.Value {
			if err := fn(m); err != nil {
				return err
			}
		}
		if lr.NextLink == "" {
			break
		}
		endpoint = lr.NextLink
	}
	return nil
}

func toMessage(m graphMessage) Message {
	var to []string
	for _, t := range m.ToRecipients {
		to = append(to, t.EmailAddress.Address)
	}

	body := m.Body.Content
	bodyHTML := ""
	if strings.ToLower(m.Body.ContentType) == "html" {
		bodyHTML = m.Body.Content
		body = stripHTML(m.Body.Content)
	}
	received, _ := time.Parse(time.RFC3339, m.ReceivedDateTime)

	return Message{
		ID:       m.ID,
		Subject:  m.Subject,
		From:     m.From.EmailAddress.Address,
		FromName: m.From.EmailAddress.Name,
		To:       to,
		Body:     body,
		BodyHTML: bodyHTML,
		Snippet:  m.BodyPreview,
		IsRead:   m.IsRead,
		Received: received,
	}
}

func stripHTML(html string) string {
	re := regexp.MustCompile("<[^>]*>")
	return re.ReplaceAllString(html, " ")
}

func (c *Client) MoveMessage(ctx context.Context, msgID, destFolderID string) error {
	payload := map[string]string{"destinationId": destFolderID}
	buf, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("%s/me/messages/%s/move", c.baseURL, msgID)
	resp, err := c.doWithRetry(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 400/404 usually means the message was deleted or moved by someone else
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return ErrMessageGone
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("move failed: %s", resp.Status)
	}
	return nil
}

// GetMessage fetches a single message by ID
func (c *Client) GetMessage(ctx context.Context, msgID string) (*Message, error) {
	params := url.Values{}
	params.Set("$select", "id,subject,from,toRecipients,body,bodyPreview,isRead,receivedDateTime,parentFolderId")
	endpoint := fmt.Sprintf("%s/me/messages/%s?%s", c.baseURL, msgID, params.Encode())
	
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("message not found: %s", msgID)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get message failed: %s - %s", resp.Status, string(body))
	}
	
	var gm graphMessage
	if err := json.NewDecoder(resp.Body).Decode(&gm); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	
	msg := toMessage(gm)
	return &msg, nil
}

// GetMessageFolder returns the folder ID containing the message
func (c *Client) GetMessageFolder(ctx context.Context, msgID string) (string, error) {
	params := url.Values{}
	params.Set("$select", "parentFolderId")
	endpoint := fmt.Sprintf("%s/me/messages/%s?%s", c.baseURL, msgID, params.Encode())
	
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("get message folder failed: %s", resp.Status)
	}
	
	var result struct {
		ParentFolderID string `json:"parentFolderId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ParentFolderID, nil
}

func (c *Client) MarkRead(ctx context.Context, msgID string) error {
	payload := map[string]bool{"isRead": true}
	buf, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("%s/me/messages/%s", c.baseURL, msgID)
	resp, err := c.doWithRetry(ctx, http.MethodPatch, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mark read failed: %s", resp.Status)
	}
	return nil
}
