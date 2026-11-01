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
	baseURL           string
	mailboxPrincipal  string             // "me" or "users/{escaped-id}" depending on auth mode
	tokenProvider     *tokenProvider     // built-in OAuth2 (preferred)
	tokenScript       string             // external script (legacy fallback)
	certTokenProvider *certTokenProvider // app-only cert auth
	useCertAsPrimary  bool               // when true, certTokenProvider is used for all requests
	httpClient        *http.Client

	sema                 chan struct{}
	maxConcurrent        int
	rangeWorkers         int
	largeFolderThreshold int
	rangeDays            int
	rng                  *rand.Rand
	rngMu                sync.Mutex

	cooldownMu    sync.Mutex
	cooldownUntil time.Time

	// Metrics
	requestCount   int64
	retryCount     int64
	requestCountMu sync.Mutex
	startTime      time.Time
}

func NewClient(cfg *config.Config) (*Client, error) {
	mailboxPrincipal, err := graphMailboxPrincipal(cfg.Graph.AuthMode, cfg.Graph.UserID)
	if err != nil {
		return nil, err
	}

	c := &Client{
		baseURL:              strings.TrimRight(cfg.Graph.BaseURL, "/"),
		mailboxPrincipal:     mailboxPrincipal,
		httpClient:           &http.Client{Timeout: 120 * time.Second},
		maxConcurrent:        cfg.Graph.MaxConcurrentRequests,
		rangeWorkers:         cfg.Graph.RangeWorkers,
		largeFolderThreshold: cfg.Graph.LargeFolderThreshold,
		rangeDays:            cfg.Graph.RangeDays,
		rng:                  rand.New(rand.NewSource(time.Now().UnixNano())),
		startTime:            time.Now(),
	}

	// Init cert token provider if configured (used for message trace, or as primary with auth_mode=cert)
	if cfg.Graph.CertPath != "" {
		ctp, err := newCertTokenProvider(
			cfg.Graph.ClientID,
			cfg.Graph.TenantID,
			cfg.Graph.CertPath,
			cfg.Graph.CertPasswordFile,
			"https://graph.microsoft.com/.default",
		)
		if err != nil {
			if cfg.Graph.AuthMode == "cert" {
				return nil, fmt.Errorf("cert token provider required by auth_mode=cert but failed: %w", err)
			}
			slog.Warn("cert token provider init failed (envelope lookup will be unavailable)", "error", err)
		} else {
			c.certTokenProvider = ctp
		}
	}

	// Choose primary auth method
	if cfg.Graph.AuthMode == "cert" {
		if c.certTokenProvider == nil {
			return nil, fmt.Errorf("auth_mode=cert requires cert_path and cert_password_file")
		}
		if cfg.Graph.UserID == "" {
			return nil, fmt.Errorf("auth_mode=cert requires user_id (app-only tokens cannot use /me)")
		}
		c.useCertAsPrimary = true
		slog.Info("using certificate auth for all Graph requests", "user", cfg.Graph.UserID)
	} else if cfg.Graph.ClientID != "" && cfg.Graph.TenantID != "" {
		tp, err := newTokenProvider(cfg.Graph.ClientID, cfg.Graph.TenantID, cfg.Graph.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("init token provider: %w", err)
		}
		c.tokenProvider = tp
	} else {
		c.tokenScript = cfg.Graph.TokenScript
	}

	if c.maxConcurrent > 0 {
		c.sema = make(chan struct{}, c.maxConcurrent)
	}

	return c, nil
}

func graphMailboxPrincipal(authMode, userID string) (string, error) {
	if authMode != "cert" {
		return "me", nil
	}
	if userID == "" {
		return "", fmt.Errorf("auth_mode=cert requires user_id (app-only tokens cannot use /me)")
	}
	return "users/" + url.PathEscape(userID), nil
}

// MailboxResource returns a Graph resource rooted at the authenticated mailbox.
// Delegated auth uses "me"; app-only auth uses the configured user mailbox.
func (c *Client) MailboxResource(resource string) string {
	resource = strings.TrimLeft(resource, "/")
	if resource == "" {
		return c.mailboxPrincipal
	}
	return c.mailboxPrincipal + "/" + resource
}

// MailFolderMessagesResource returns the subscription resource for a mail folder.
func (c *Client) MailFolderMessagesResource(folderID string) string {
	folderID = strings.ReplaceAll(folderID, "'", "''")
	return c.MailboxResource(fmt.Sprintf("mailFolders('%s')/messages", folderID))
}

func (c *Client) mailboxURL(resource string) string {
	return c.baseURL + "/" + c.MailboxResource(resource)
}

// Metrics returns current request stats
func (c *Client) Metrics() (requests int64, retries int64, elapsed time.Duration, reqPerMin float64) {
	c.requestCountMu.Lock()
	requests = c.requestCount
	retries = c.retryCount
	c.requestCountMu.Unlock()
	elapsed = time.Since(c.startTime)
	if elapsed.Minutes() > 0 {
		reqPerMin = float64(requests) / elapsed.Minutes()
	}
	return
}

// ResetMetrics resets the counters and start time
func (c *Client) ResetMetrics() {
	c.requestCountMu.Lock()
	c.requestCount = 0
	c.retryCount = 0
	c.startTime = time.Now()
	c.requestCountMu.Unlock()
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
	Headers  map[string]string
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
	BodyPreview            string `json:"bodyPreview"`
	IsRead                 bool   `json:"isRead"`
	ReceivedDateTime       string `json:"receivedDateTime"`
	InternetMessageHeaders []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"internetMessageHeaders"`
}

type listResponse struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

func (c *Client) token(ctx context.Context) (string, error) {
	if c.useCertAsPrimary && c.certTokenProvider != nil {
		return c.certTokenProvider.getToken(ctx)
	}
	if c.tokenProvider != nil {
		return c.tokenProvider.getToken(ctx)
	}
	return tokenFromScript(ctx, c.tokenScript)
}

// GetToken returns a valid access token (public wrapper for subscription manager)
func (c *Client) GetToken() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), graphRetryBudget)
	defer cancel()
	return c.token(ctx)
}

// HasCertAuth returns true if cert-based app-only auth is configured (for message trace).
func (c *Client) HasCertAuth() bool {
	return c.certTokenProvider != nil
}

// graphRetryBudget bounds authentication, queueing, cooldowns, attempts, and body reads.
const graphRetryBudget = 5 * time.Minute

// responseBody keeps request resources until the caller closes the response.
type responseBody struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
	cancel  context.CancelFunc
	err     error
}

func (b *responseBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.onClose()
		if b.cancel != nil {
			b.cancel()
		}
	})
	return b.err
}

func (c *Client) extendCooldown(delay time.Duration) {
	until := time.Now().Add(delay)
	c.cooldownMu.Lock()
	if until.After(c.cooldownUntil) {
		c.cooldownUntil = until
	}
	c.cooldownMu.Unlock()
}

func (c *Client) waitCooldown(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.cooldownMu.Lock()
		delay := time.Until(c.cooldownUntil)
		c.cooldownMu.Unlock()
		if delay <= 0 {
			return nil
		}
		if err := waitRetry(ctx, delay); err != nil {
			return err
		}
		// Another response may have extended the mailbox cooldown while we waited.
	}
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := c.waitCooldown(ctx); err != nil {
		return nil, err
	}
	release := func() {}
	if c.sema != nil {
		select {
		case c.sema <- struct{}{}:
			release = func() { <-c.sema }
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// A throttle may have arrived while this request was queued for a permit.
	if err := c.waitCooldown(ctx); err != nil {
		release()
		return nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		release()
		return nil, err
	}
	// Authentication can block while another request establishes a cooldown.
	if err := c.waitCooldown(ctx); err != nil {
		release()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &responseBody{ReadCloser: resp.Body, onClose: release}
	return resp, nil
}

func (c *Client) doWithRetry(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	const maxRetries = 5
	ctx, cancel := context.WithTimeout(ctx, graphRetryBudget)
	returned := false
	defer func() {
		if !returned {
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for attempt := 0; ; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			req.Body, err = req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("recreate Graph request body: %w", err)
			}
		}
		c.requestCountMu.Lock()
		c.requestCount++
		if attempt > 0 {
			c.retryCount++
		}
		c.requestCountMu.Unlock()

		resp, err := c.do(req)
		if err != nil {
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, err
		}
		retryableStatus := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		delay := time.Duration(0)
		sharedDelay := false
		if retryableStatus {
			if retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				// Retry-After is a server minimum, never subject to negative jitter.
				delay = retryAfter
				sharedDelay = true
			} else {
				delay = c.jitter(time.Second * time.Duration(1<<attempt))
				sharedDelay = resp.StatusCode == http.StatusTooManyRequests
			}
			if sharedDelay {
				// Publish even on the final attempt, or for a non-replayable request.
				c.extendCooldown(delay)
			}
		}
		// A 429 explicitly rejects the operation. A 5xx may follow a committed
		// mutation, so only automatically replay methods with idempotent semantics.
		safeMethod := method == http.MethodGet || method == http.MethodHead ||
			method == http.MethodOptions || method == http.MethodPut || method == http.MethodDelete
		replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
		if !retryableStatus || !replayable || (!safeMethod && resp.StatusCode != http.StatusTooManyRequests) {
			resp.Body.(*responseBody).cancel = cancel
			returned = true
			return resp, nil
		}
		resp.Body.Close()
		if attempt >= maxRetries {
			return nil, fmt.Errorf("request failed after retries: %s", resp.Status)
		}
		slog.Warn("graph request retry", "status", resp.Status, "attempt", attempt+1, "delay", delay, "url", url)
		if !sharedDelay {
			if err := waitRetry(ctx, delay); err != nil {
				return nil, err
			}
		}
		// Shared delays are enforced in do, including any later extension.
	}
}

func parseRetryAfter(val string) (time.Duration, bool) {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0, false
	}
	// Parse unsigned decimal seconds without allowing a sign or duration overflow.
	numeric := true
	for _, ch := range val {
		if ch < '0' || ch > '9' {
			numeric = false
			break
		}
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if numeric {
		secs, err := strconv.ParseUint(val, 10, 64)
		if err != nil || secs > uint64(maxDuration/time.Second) {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(val); err == nil {
		now := time.Now()
		if t.After(now.Add(maxDuration)) {
			return 0, false
		}
		d := t.Sub(now)
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
		endpoint = c.mailboxURL("mailFolders")
	} else {
		endpoint = c.mailboxURL(fmt.Sprintf("mailFolders/%s/childFolders", parentID))
	}

	reqURL := endpoint + "?$top=200"
	for {
		resp, err := c.doWithRetry(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return "", err
		}
		// Close each page before requesting another: permits cover open bodies.
		if resp.StatusCode >= 300 {
			resp.Body.Close()
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
			resp.Body.Close()
			return "", err
		}
		resp.Body.Close()
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
	endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s/childFolders?$top=200", parentID))
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
	OnlyUnread   bool
	Since        time.Duration
	Before       time.Time
	Fields       []string
	Fast         bool
	SenderFilter string // Graph API filter for from/emailAddress/address (supports * wildcards)
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
// senderPatternToFilter converts a glob-style sender pattern to Graph API query parameters.
// Returns (filterStr, searchStr):
//   - filterStr: $filter clause for exact matches
//   - searchStr: KQL search string for wildcard patterns (use with $search)
//
// Supported patterns:
//   - "user@domain.com" → filter: "from/emailAddress/address eq 'user@domain.com'"
//   - "*@domain.com" → search: "from:domain.com"
//   - "user@*" → search: "from:user@"
//   - "*pattern*@*" → search: "from:pattern" (best effort, requires client-side verify)
//
// Note: Graph API $search is fuzzy and may over-match. Always verify results client-side.
func senderPatternToFilter(pattern string) (filterStr, searchStr string) {
	if pattern == "" {
		return "", ""
	}

	// Lowercase for case-insensitive matching
	pattern = strings.ToLower(pattern)

	// Exact match: use $filter (most precise)
	if !strings.Contains(pattern, "*") {
		return fmt.Sprintf("from/emailAddress/address eq '%s'", pattern), ""
	}

	// Wildcard patterns: use $search with KQL from: syntax
	// KQL from: does substring matching on sender addresses

	// Pattern: *@domain.com → search: "from:domain.com"
	if strings.HasPrefix(pattern, "*@") && !strings.Contains(pattern[2:], "*") {
		domain := pattern[2:] // Strip leading *@
		return "", fmt.Sprintf("from:%s", domain)
	}

	// Pattern: user@* → search: "from:user@"
	if strings.HasSuffix(pattern, "@*") && !strings.Contains(pattern[:len(pattern)-2], "*") {
		localPart := pattern[:len(pattern)-1] // Strip trailing * but keep @
		return "", fmt.Sprintf("from:%s", localPart)
	}

	// Pattern: *pattern*@* or other complex wildcards → extract best search term
	// Find the longest non-* segment to search for
	parts := strings.Split(pattern, "*")
	var longestPart string
	for _, part := range parts {
		// Skip @-only parts and very short parts
		if part != "" && part != "@" && len(part) > len(longestPart) {
			longestPart = part
		}
	}
	if longestPart != "" {
		return "", fmt.Sprintf("from:%s", longestPart)
	}

	// Pattern like *@* — can't narrow down at all, must scan everything
	return "", ""
}

// matchSenderPattern checks if an email address matches a glob pattern.
// Used for client-side verification of $search results.
func matchSenderPattern(pattern, email string) bool {
	if pattern == "" {
		return true
	}

	// Case-insensitive matching
	pattern = strings.ToLower(pattern)
	email = strings.ToLower(email)

	// Convert glob to regex
	escaped := regexp.QuoteMeta(pattern)
	escaped = strings.ReplaceAll(escaped, "\\*", ".*")
	re, err := regexp.Compile("^" + escaped + "$")
	if err != nil {
		return false
	}
	return re.MatchString(email)
}

func (c *Client) StreamMessages(ctx context.Context, folderID string, opts ListOptions, fn func(msg Message) error) error {
	params := url.Values{}
	params.Set("$top", "50")
	params.Set("$orderby", "receivedDateTime desc")
	params.Set("$select", buildSelect(opts))

	var filtersNoDate []string
	if opts.OnlyUnread {
		filtersNoDate = append(filtersNoDate, "isRead eq false")
	}

	// Check if we need to use $search for sender pattern
	var senderFilter, senderSearch string
	var senderPattern string // For client-side verification
	if opts.SenderFilter != "" {
		senderPattern = opts.SenderFilter
		senderFilter, senderSearch = senderPatternToFilter(opts.SenderFilter)
		if senderFilter != "" {
			filtersNoDate = append(filtersNoDate, senderFilter)
			slog.Info("using server-side sender filter", "pattern", opts.SenderFilter, "filter", senderFilter)
		} else if senderSearch != "" {
			slog.Info("using server-side sender search", "pattern", opts.SenderFilter, "search", senderSearch)
		} else {
			slog.Info("sender pattern requires full client-side filtering", "pattern", opts.SenderFilter)
		}
	}

	var sinceTime time.Time
	if opts.Since > 0 {
		sinceTime = time.Now().Add(-opts.Since).UTC()
	}

	beforeTime := opts.Before
	if !beforeTime.IsZero() {
		beforeTime = beforeTime.UTC()
	}

	// When using $search, we can't combine with $filter or $orderby.
	// Use simple pagination path with client-side filtering for dates.
	if senderSearch != "" {
		// Graph API $search requires the value to be double-quoted
		params.Set("$search", fmt.Sprintf("\"%s\"", senderSearch))
		params.Del("$orderby") // $search doesn't support $orderby

		// If there are other filters (like OnlyUnread), we can't use them with $search
		// Log a warning and apply them client-side
		if len(filtersNoDate) > 0 {
			slog.Warn("$search cannot combine with $filter, applying filters client-side",
				"filters", filtersNoDate)
		}

		// Warn about date filtering being client-side
		if !sinceTime.IsZero() || !beforeTime.IsZero() {
			slog.Info("date filtering will be applied client-side (cannot combine with $search)")
		}

		endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s/messages?%s", folderID, params.Encode()))
		var resultCount int
		err := c.streamMessagesEndpoint(ctx, endpoint, func(m graphMessage) error {
			resultCount++
			msg := toMessage(m)

			// Client-side sender verification (search is fuzzy)
			if senderPattern != "" && !matchSenderPattern(senderPattern, msg.From) {
				return nil // Skip non-matching
			}

			// Client-side date filtering
			if !sinceTime.IsZero() && msg.Received.Before(sinceTime) {
				return nil
			}
			if !beforeTime.IsZero() && !msg.Received.Before(beforeTime) {
				return nil
			}

			// Client-side OnlyUnread filter
			if opts.OnlyUnread && msg.IsRead {
				return nil
			}

			return fn(msg)
		})

		// Warn if we hit the Graph API $search limit
		if resultCount >= 1000 {
			slog.Warn("$search results may be incomplete (hit 1000 result limit)",
				"pattern", opts.SenderFilter, "results", resultCount)
		}

		return err
	}

	// Sender-filter path: when filtering by from/emailAddress/address, Graph rejects
	// $orderby on a different property (receivedDateTime) with InefficientFilter / 400.
	// Per Graph docs: properties in $orderby must also appear in $filter, in the same order.
	// Drop $orderby and skip date-range batching — the sender filter narrows results
	// enough that simple pagination is fine. Date constraints are still applied server-side
	// as additional $filter clauses (filter-only is fine without orderby).
	// See: https://learn.microsoft.com/graph/api/user-list-messages#using-filter-and-orderby-in-the-same-query
	if senderFilter != "" {
		params.Del("$orderby")
		filters := append([]string{}, filtersNoDate...)
		if !sinceTime.IsZero() {
			filters = append(filters, fmt.Sprintf("receivedDateTime ge %s", sinceTime.Format(time.RFC3339)))
		}
		if !beforeTime.IsZero() {
			filters = append(filters, fmt.Sprintf("receivedDateTime lt %s", beforeTime.Format(time.RFC3339)))
		}
		if len(filters) > 0 {
			params.Set("$filter", strings.Join(filters, " and "))
		}
		endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s/messages?%s", folderID, params.Encode()))
		return c.streamMessagesEndpoint(ctx, endpoint, func(m graphMessage) error {
			return fn(toMessage(m))
		})
	}

	// Standard path: use $filter (batched or simple pagination)
	if c.shouldBatch(ctx, folderID) {
		start, end, ok, err := c.getDateBounds(ctx, folderID, filtersNoDate, sinceTime, beforeTime)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		ranges := buildDateRanges(start, end, c.rangeDays)
		resultsCh := make(chan []Message, c.rangeWorkers)
		errCh := make(chan error, 1)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		g, gctx := errgroup.WithContext(ctx)
		sem := make(chan struct{}, c.rangeWorkers)
		for _, r := range ranges {
			r := r
			g.Go(func() error {
				sem <- struct{}{}
				defer func() { <-sem }()
				msgs, err := c.listMessagesRange(gctx, folderID, params, filtersNoDate, r.Start, r.End)
				if err != nil {
					return err
				}
				select {
				case resultsCh <- msgs:
					return nil
				case <-gctx.Done():
					return gctx.Err()
				}
			})
		}

		go func() {
			errCh <- g.Wait()
			close(resultsCh)
		}()

		for msgs := range resultsCh {
			for _, msg := range msgs {
				if err := fn(msg); err != nil {
					cancel()
					return err
				}
			}
		}
		if err := <-errCh; err != nil {
			return err
		}
		return nil
	}

	filters := append([]string{}, filtersNoDate...)
	if !sinceTime.IsZero() {
		filters = append(filters, fmt.Sprintf("receivedDateTime ge %s", sinceTime.Format(time.RFC3339)))
	}
	if !beforeTime.IsZero() {
		filters = append(filters, fmt.Sprintf("receivedDateTime lt %s", beforeTime.Format(time.RFC3339)))
	}
	if len(filters) > 0 {
		params.Set("$filter", strings.Join(filters, " and "))
	}

	endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s/messages?%s", folderID, params.Encode()))
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
		fields = []string{"id", "subject", "from", "toRecipients", "body", "bodyPreview", "isRead", "receivedDateTime", "internetMessageHeaders"}
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
	endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s?$select=totalItemCount", folderID))
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

func (c *Client) getDateBounds(ctx context.Context, folderID string, filters []string, since time.Time, before time.Time) (time.Time, time.Time, bool, error) {
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
	if !before.IsZero() && before.Before(endExclusive) {
		endExclusive = before
	}
	if endExclusive.Before(start) || endExclusive.Equal(start) {
		return time.Time{}, time.Time{}, false, nil
	}
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
	endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s/messages?%s", folderID, params.Encode()))
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
	endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/%s/messages?%s", folderID, params.Encode()))
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

	// Convert internet message headers to a map. Preserve repeated fields such
	// as References by joining their values instead of dropping earlier ones.
	headers := make(map[string]string)
	for _, h := range m.InternetMessageHeaders {
		if existing := headers[h.Name]; existing != "" {
			headers[h.Name] = existing + " " + h.Value
		} else {
			headers[h.Name] = h.Value
		}
	}

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
		Headers:  headers,
	}
}

func stripHTML(html string) string {
	re := regexp.MustCompile("<[^>]*>")
	return re.ReplaceAllString(html, " ")
}

// MoveMessage moves a message to a destination folder and returns the new message ID.
// Graph API assigns a new ID to messages when they are moved.
func (c *Client) MoveMessage(ctx context.Context, msgID, destFolderID string) (string, error) {
	payload := map[string]string{"destinationId": destFolderID}
	buf, _ := json.Marshal(payload)
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s/move", msgID))
	resp, err := c.doWithRetry(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 400/404 usually means the message was deleted or moved by someone else
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return "", ErrMessageGone
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("move failed: %s", resp.Status)
	}
	// Parse response to get new message ID
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// If we can't parse the response, return original ID as fallback
		return msgID, nil
	}
	if result.ID == "" {
		return msgID, nil
	}
	return result.ID, nil
}

// GetMessage fetches a single message by ID
func (c *Client) GetMessage(ctx context.Context, msgID string) (*Message, error) {
	params := url.Values{}
	params.Set("$select", "id,subject,from,toRecipients,body,bodyPreview,isRead,receivedDateTime,parentFolderId,internetMessageHeaders")
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s?%s", msgID, params.Encode()))

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

// GetMessageByInternetMessageID finds a message by its RFC 5322 Message-ID header.
// The messageID should include angle brackets, e.g. "<abc123@example.com>"
func (c *Client) GetMessageByInternetMessageID(ctx context.Context, messageID string) (*Message, error) {
	params := url.Values{}
	params.Set("$select", "id,subject,from,toRecipients,body,bodyPreview,isRead,receivedDateTime,parentFolderId,internetMessageId,internetMessageHeaders")
	params.Set("$filter", fmt.Sprintf("internetMessageId eq '%s'", messageID))
	params.Set("$top", "1")
	endpoint := c.mailboxURL(fmt.Sprintf("messages?%s", params.Encode()))

	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search by internetMessageId failed: %s - %s", resp.Status, string(body))
	}

	var result struct {
		Value []graphMessage `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Value) == 0 {
		return nil, fmt.Errorf("message not found with internetMessageId: %s", messageID)
	}

	msg := toMessage(result.Value[0])
	return &msg, nil
}

// HasSentMessageByInternetMessageID reports whether Sent Items contains a
// message with any of the supplied RFC 5322 Message-IDs.
func (c *Client) HasSentMessageByInternetMessageID(ctx context.Context, messageIDs []string) (bool, error) {
	for _, messageID := range messageIDs {
		params := url.Values{}
		params.Set("$select", "id")
		params.Set("$filter", fmt.Sprintf("internetMessageId eq '%s'", strings.ReplaceAll(messageID, "'", "''")))
		params.Set("$top", "1")
		endpoint := c.mailboxURL(fmt.Sprintf("mailFolders/sentitems/messages?%s", params.Encode()))

		resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return false, err
		}

		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return false, fmt.Errorf("search Sent Items by internetMessageId failed: %s - %s", resp.Status, string(body))
		}

		var result struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return false, fmt.Errorf("decode Sent Items search response: %w", err)
		}
		resp.Body.Close()
		if len(result.Value) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// GetMessageFolder returns the folder ID containing the message
func (c *Client) GetMessageFolder(ctx context.Context, msgID string) (string, error) {
	params := url.Values{}
	params.Set("$select", "parentFolderId")
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s?%s", msgID, params.Encode()))

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
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s", msgID))
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

// SetImportance sets the importance of a message.
func (c *Client) SetImportance(ctx context.Context, msgID, importance string) error {
	payload := map[string]string{"importance": importance}
	buf, _ := json.Marshal(payload)
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s", msgID))
	resp, err := c.doWithRetry(ctx, http.MethodPatch, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("set importance failed: %s - %s", resp.Status, string(body))
	}
	return nil
}

// FlagMessage sets the follow-up flag on a message.
// status can be: "flagged", "complete", or "notFlagged"
func (c *Client) FlagMessage(ctx context.Context, msgID string, status string) error {
	payload := map[string]interface{}{
		"flag": map[string]string{
			"flagStatus": status,
		},
	}
	buf, _ := json.Marshal(payload)
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s", msgID))
	resp, err := c.doWithRetry(ctx, http.MethodPatch, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("flag message failed: %s - %s", resp.Status, string(body))
	}
	return nil
}

// GetCategories returns the current categories on a message.
func (c *Client) GetCategories(ctx context.Context, msgID string) ([]string, error) {
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s?$select=categories", msgID))
	resp, err := c.doWithRetry(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get categories failed: %s - %s", resp.Status, string(body))
	}
	var result struct {
		Categories []string `json:"categories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Categories, nil
}

// AddCategories merges categories into the message's existing categories.
// Existing categories retain their order and are never removed.
func (c *Client) AddCategories(ctx context.Context, msgID string, categories []string) ([]string, error) {
	current, err := c.GetCategories(ctx, msgID)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(current)+len(categories))
	merged := make([]string, 0, len(current)+len(categories))
	for _, category := range current {
		if _, ok := seen[category]; ok {
			continue
		}
		seen[category] = struct{}{}
		merged = append(merged, category)
	}
	for _, category := range categories {
		if _, ok := seen[category]; ok {
			continue
		}
		seen[category] = struct{}{}
		merged = append(merged, category)
	}

	if err := c.SetCategories(ctx, msgID, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// RemoveCategory removes a single category from a message, preserving others.
// Returns the remaining categories.
func (c *Client) RemoveCategory(ctx context.Context, msgID string, category string) ([]string, error) {
	current, err := c.GetCategories(ctx, msgID)
	if err != nil {
		return nil, err
	}
	// Initialize as non-nil so JSON marshals to [] rather than null when
	// the removed category was the only one present. Microsoft Graph rejects
	// PATCH {"categories": null} with 400 RequestBodyRead even though the
	// schema declares the collection nullable.
	remaining := []string{}
	for _, cat := range current {
		if cat != category {
			remaining = append(remaining, cat)
		}
	}
	if err := c.SetCategories(ctx, msgID, remaining); err != nil {
		return nil, err
	}
	return remaining, nil
}

// SetCategories sets the categories on a message.
// Categories are colored labels in Outlook (e.g., "Red Category", "Blue Category", or custom names).
func (c *Client) SetCategories(ctx context.Context, msgID string, categories []string) error {
	// Coerce a nil slice to an empty slice so it marshals as [] not null.
	// Microsoft Graph PATCH /messages/{id} rejects {"categories": null} with
	// 400 RequestBodyRead ("A null value was found for the property named
	// 'categories'..."), but accepts {"categories": []} to clear all
	// categories. The collection is allowed to *contain* nulls but not
	// to *be* null.
	if categories == nil {
		categories = []string{}
	}
	payload := map[string]interface{}{
		"categories": categories,
	}
	buf, _ := json.Marshal(payload)
	endpoint := c.mailboxURL(fmt.Sprintf("messages/%s", msgID))
	resp, err := c.doWithRetry(ctx, http.MethodPatch, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("set categories failed: %s - %s", resp.Status, string(body))
	}
	return nil
}
