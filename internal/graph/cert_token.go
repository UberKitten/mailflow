package graph

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pkcs12"
)

// certTokenProvider acquires app-only (client_credentials) tokens using a certificate.
// Used for APIs that require application permissions (e.g. message trace).
type certTokenProvider struct {
	clientID string
	tenantID string
	certPath string
	certPass string
	scope    string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	privKey *rsa.PrivateKey
	certDER []byte // raw certificate bytes for x5t thumbprint
}

func newCertTokenProvider(clientID, tenantID, certPath, certPasswordFile, scope string) (*certTokenProvider, error) {
	pass := ""
	if certPasswordFile != "" {
		data, err := os.ReadFile(certPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read cert password file: %w", err)
		}
		pass = strings.TrimSpace(string(data))
	}

	pfxData, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read cert file: %w", err)
	}

	privKey, certDER, err := parsePFX(pfxData, pass)
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}

	return &certTokenProvider{
		clientID: clientID,
		tenantID: tenantID,
		certPath: certPath,
		certPass: pass,
		scope:    scope,
		privKey:  privKey,
		certDER:  certDER,
	}, nil
}

func parsePFX(pfxData []byte, password string) (*rsa.PrivateKey, []byte, error) {
	blocks, err := pkcs12.ToPEM(pfxData, password)
	if err != nil {
		return nil, nil, fmt.Errorf("decode PFX: %w", err)
	}

	var privKey *rsa.PrivateKey
	var certDER []byte

	for _, block := range blocks {
		switch block.Type {
		case "PRIVATE KEY":
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				continue
			}
			if rsaKey, ok := key.(*rsa.PrivateKey); ok {
				privKey = rsaKey
			}
		case "RSA PRIVATE KEY":
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				continue
			}
			privKey = key
		case "CERTIFICATE":
			if certDER == nil {
				certDER = block.Bytes
			}
		}
	}

	if privKey == nil {
		return nil, nil, fmt.Errorf("no RSA private key found in PFX")
	}
	if certDER == nil {
		return nil, nil, fmt.Errorf("no certificate found in PFX")
	}

	return privKey, certDER, nil
}

func (p *certTokenProvider) getToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.accessToken != "" && time.Now().Add(5*time.Minute).Before(p.expiresAt) {
		return p.accessToken, nil
	}

	return p.refresh()
}

func (p *certTokenProvider) refresh() (string, error) {
	assertion, err := p.buildAssertion()
	if err != nil {
		return "", fmt.Errorf("build JWT assertion: %w", err)
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", p.tenantID)

	form := url.Values{
		"client_id":             {p.clientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
		"grant_type":            {"client_credentials"},
		"scope":                 {p.scope},
	}

	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		return "", fmt.Errorf("cert token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read cert token response: %w", err)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse cert token response: %w", err)
	}

	if tokenResp.Error != "" {
		return "", fmt.Errorf("cert token failed: %s: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("cert token returned empty access_token")
	}

	p.accessToken = tokenResp.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return p.accessToken, nil
}

func (p *certTokenProvider) buildAssertion() (string, error) {
	// x5t: base64url-encoded SHA-1 thumbprint of the certificate
	thumbprint := sha1.Sum(p.certDER)
	x5t := base64.RawURLEncoding.EncodeToString(thumbprint[:])

	now := time.Now().UTC()
	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
		"x5t": x5t,
	}

	aud := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", p.tenantID)
	payload := map[string]interface{}{
		"aud": aud,
		"iss": p.clientID,
		"sub": p.clientID,
		"jti": fmt.Sprintf("%d", now.UnixNano()),
		"nbf": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	sigInput := headerB64 + "." + payloadB64
	hash := sha256.Sum256([]byte(sigInput))

	sig, err := rsa.SignPKCS1v15(rand.Reader, p.privKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return sigInput + "." + sigB64, nil
}

// decodePEM is a helper to extract PEM blocks from raw data.
func decodePEM(data []byte) []*pem.Block {
	var blocks []*pem.Block
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
		data = rest
	}
	return blocks
}
