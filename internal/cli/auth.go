package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"mailflow/internal/config"

	"github.com/spf13/cobra"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authenticate with Microsoft Graph API",
	Long: `Perform the OAuth2 authorization code flow to obtain a refresh token.

Opens your browser to sign in with your Microsoft account. After authorization,
the token is saved to the token file (default: .ms-graph-token.json in config dir).

Requires client_id and tenant_id in config.yaml.`,
	RunE: runAuth,
}

func init() {
	rootCmd.AddCommand(authCmd)
}

func runAuth(cmd *cobra.Command, args []string) error {
	cfgDir, _ := cmd.Root().Flags().GetString("config-dir")

	cfg, err := config.LoadMainConfig(cfgDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Graph.ClientID == "" || cfg.Graph.TenantID == "" {
		return fmt.Errorf("client_id and tenant_id must be set in config.yaml")
	}

	tokenFile := cfg.Graph.TokenFile
	if tokenFile == "" {
		tokenFile = filepath.Join(cfgDir, ".ms-graph-token.json")
	}

	// Check if already authenticated
	if _, err := os.Stat(tokenFile); err == nil {
		fmt.Printf("Token file already exists: %s\n", tokenFile)
		fmt.Print("Re-authenticate? (y/N): ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Start local callback server
	listener, err := net.Listen("tcp", "127.0.0.1:8400")
	if err != nil {
		return fmt.Errorf("start callback server: %w", err)
	}
	defer listener.Close()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error_description")
			if errMsg == "" {
				errMsg = r.URL.Query().Get("error")
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "<h2>Authentication failed</h2><p>%s</p>", errMsg)
			errCh <- fmt.Errorf("auth failed: %s", errMsg)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h2>✓ Authenticated!</h2><p>You can close this tab.</p>")
		codeCh <- code
	})

	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Shutdown(context.Background())

	// Build authorization URL
	authURL := fmt.Sprintf(
		"https://login.microsoftonline.com/%s/oauth2/v2.0/authorize?"+
			"client_id=%s&response_type=code&redirect_uri=%s&scope=%s",
		cfg.Graph.TenantID,
		cfg.Graph.ClientID,
		url.QueryEscape("http://localhost:8400/callback"),
		url.QueryEscape("offline_access Mail.ReadWrite"),
	)

	fmt.Printf("\nOpen this URL in your browser to sign in:\n\n")
	fmt.Println(authURL)
	fmt.Printf("\nWaiting for authorization...\n")

	// Wait for callback
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for authorization")
	}

	slog.Info("received authorization code, exchanging for token")

	// Exchange code for tokens
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", cfg.Graph.TenantID)
	resp, err := http.PostForm(tokenURL, url.Values{
		"client_id":    {cfg.Graph.ClientID},
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {"http://localhost:8400/callback"},
		"scope":        {"offline_access Mail.ReadWrite"},
	})
	if err != nil {
		return fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}

	if tokenResp.Error != "" {
		return fmt.Errorf("token exchange failed: %s: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	if tokenResp.RefreshToken == "" {
		return fmt.Errorf("no refresh token in response (did you include offline_access scope?)")
	}

	// Save token file (atomic write)
	tokenData, _ := json.Marshal(tokenResp)
	tmpFile := tokenFile + ".tmp"
	if err := os.WriteFile(tmpFile, tokenData, 0600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if err := os.Rename(tmpFile, tokenFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("save token file: %w", err)
	}

	fmt.Printf("\n✓ Token saved to %s\n", tokenFile)
	fmt.Println("Mailflow will automatically refresh the token before it expires.")
	return nil
}
