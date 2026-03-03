package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// tokenProvider handles OAuth2 token refresh for Microsoft Graph API.
// It caches tokens in a JSON file and refreshes them before expiry.
type tokenProvider struct {
	clientID  string
	tenantID  string
	tokenFile string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func newTokenProvider(clientID, tenantID, tokenFile string) (*tokenProvider, error) {
	tp := &tokenProvider{
		clientID:  clientID,
		tenantID:  tenantID,
		tokenFile: tokenFile,
	}

	// Load cached token if available
	if err := tp.loadCached(); err != nil {
		return nil, fmt.Errorf("load token cache %s: %w", tokenFile, err)
	}

	return tp, nil
}

func (tp *tokenProvider) getToken() (string, error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	// Return cached token if still valid (with 5 min buffer)
	if tp.accessToken != "" && time.Now().Add(5*time.Minute).Before(tp.expiresAt) {
		return tp.accessToken, nil
	}

	// Need to refresh
	if err := tp.refresh(); err != nil {
		return "", err
	}

	return tp.accessToken, nil
}

func (tp *tokenProvider) loadCached() error {
	data, err := os.ReadFile(tp.tokenFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("token file not found: %s (run 'mailflow auth' to set up)", tp.tokenFile)
		}
		return err
	}

	var cached tokenResponse
	if err := json.Unmarshal(data, &cached); err != nil {
		return fmt.Errorf("parse token file: %w", err)
	}

	if cached.RefreshToken == "" {
		return fmt.Errorf("no refresh_token in %s", tp.tokenFile)
	}

	// Try to use the cached access token if we can estimate expiry from file mtime
	if cached.AccessToken != "" && cached.ExpiresIn > 0 {
		info, err := os.Stat(tp.tokenFile)
		if err == nil {
			tp.expiresAt = info.ModTime().Add(time.Duration(cached.ExpiresIn) * time.Second)
			if time.Now().Add(5 * time.Minute).Before(tp.expiresAt) {
				tp.accessToken = cached.AccessToken
			}
		}
	}

	return nil
}

func (tp *tokenProvider) refresh() error {
	// Read current refresh token from file
	data, err := os.ReadFile(tp.tokenFile)
	if err != nil {
		return fmt.Errorf("read token file for refresh: %w", err)
	}

	var cached tokenResponse
	if err := json.Unmarshal(data, &cached); err != nil {
		return fmt.Errorf("parse token file for refresh: %w", err)
	}

	if cached.RefreshToken == "" {
		return fmt.Errorf("no refresh_token in %s", tp.tokenFile)
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tp.tenantID)

	form := url.Values{
		"client_id":     {tp.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {cached.RefreshToken},
	}

	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		return fmt.Errorf("token refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}

	if tokenResp.Error != "" {
		return fmt.Errorf("token refresh failed: %s: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("token refresh returned empty access_token")
	}

	// Save new token to file (includes new refresh_token for next time)
	newData, err := json.Marshal(tokenResp)
	if err != nil {
		return fmt.Errorf("marshal token response: %w", err)
	}

	// Atomic write: temp file + rename
	tmpFile := tp.tokenFile + ".tmp"
	if err := os.WriteFile(tmpFile, newData, 0600); err != nil {
		return fmt.Errorf("write token temp file: %w", err)
	}
	if err := os.Rename(tmpFile, tp.tokenFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("rename token file: %w", err)
	}

	tp.accessToken = tokenResp.AccessToken
	tp.expiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return nil
}

// tokenFromScript gets a token by executing an external script (legacy mode).
func tokenFromScript(scriptPath string) (string, error) {
	cmd := exec.Command(filepath.Clean(scriptPath))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token script failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
