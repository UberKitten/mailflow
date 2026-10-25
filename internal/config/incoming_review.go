package config

import (
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// IncomingReviewConfig enables metadata-only capture independently of sorting.
type IncomingReviewConfig struct {
	Mailbox         string `yaml:"mailbox"`
	MailboxObjectID string `yaml:"mailbox_object_id"`
	Path            string `yaml:"path"`
	ExternalURL     string `yaml:"external_url"`
	SpoolDir        string `yaml:"spool_dir"`
}

func (c *IncomingReviewConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("incoming_review must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "mailbox", "mailbox_object_id", "path", "external_url", "spool_dir":
		default:
			return fmt.Errorf("unknown incoming_review key %q", node.Content[i].Value)
		}
	}
	type plain IncomingReviewConfig
	return node.Decode((*plain)(c))
}

func (c *Config) validateIncomingReview(configDir string) error {
	r := c.IncomingReview
	if r == nil {
		return nil
	}
	r.Mailbox = strings.ToLower(strings.TrimSpace(r.Mailbox))
	address, err := mail.ParseAddress(r.Mailbox)
	if err != nil || address.Address != r.Mailbox {
		return fmt.Errorf("incoming_review.mailbox must be an explicit email address without a display name")
	}
	if !validMailboxObjectID(r.MailboxObjectID) {
		return fmt.Errorf("incoming_review.mailbox_object_id must be the canonical mailbox UUID")
	}
	r.MailboxObjectID = strings.ToLower(r.MailboxObjectID)
	if !c.Webhook.Enabled || c.Graph.AuthMode != "cert" || c.Graph.ClientID == "" || c.Graph.TenantID == "" || c.Graph.CertPath == "" {
		return fmt.Errorf("incoming_review requires enabled webhook and certificate app authentication with client_id, tenant_id and cert_path")
	}
	if c.Webhook.RetryIntervalSeconds <= 0 {
		return fmt.Errorf("incoming_review requires a positive webhook.retry_interval_seconds")
	}
	primaryPath := c.Webhook.Path
	if primaryPath == "" {
		primaryPath = "/webhook"
	}
	if r.Path == "" || r.Path == "/" || r.Path == primaryPath || r.Path == "/health" || strings.HasSuffix(r.Path, "/") || strings.Contains(r.Path, "//") || !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("incoming_review.path must be a distinct non-root HTTP path")
	}
	for _, ch := range r.Path {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("/-_", ch)) {
			return fmt.Errorf("incoming_review.path contains unsupported characters")
		}
	}
	primary, err := url.Parse(c.Webhook.ExternalURL)
	if err != nil || primary.Scheme != "https" || primary.Host == "" || primary.User != nil || primary.RawQuery != "" || primary.ForceQuery || primary.Fragment != "" {
		return fmt.Errorf("incoming_review requires a valid HTTPS webhook.external_url")
	}
	external, err := url.Parse(r.ExternalURL)
	if err != nil || external.Scheme != primary.Scheme || external.Host != primary.Host || external.User != nil || external.RawQuery != "" || external.ForceQuery || external.Fragment != "" || external.RawPath != "" || external.Path != r.Path || external.Path == primary.Path {
		return fmt.Errorf("incoming_review.external_url must use the webhook origin and incoming_review.path")
	}
	if r.SpoolDir == "" {
		return fmt.Errorf("incoming_review.spool_dir is required")
	}
	base, err := filepath.Abs(configDir)
	if err != nil {
		return err
	}
	spool := r.SpoolDir
	if !filepath.IsAbs(spool) {
		spool = filepath.Join(base, spool)
	}
	spool = filepath.Clean(spool)
	privateRoot := filepath.Join(base, ".incoming-review")
	rel, err := filepath.Rel(privateRoot, spool)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("incoming_review.spool_dir must be inside the config .incoming-review subtree")
	}
	for current := spool; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect incoming review spool: %w", err)
		}
		if err == nil && (!info.IsDir() || info.Mode().Perm()&0o077 != 0) {
			return fmt.Errorf("incoming review spool subtree must contain only private directories, not symlinks")
		}
		if current == privateRoot {
			break
		}
	}
	r.SpoolDir = spool
	return nil
}

func validMailboxObjectID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}
		} else if !((ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') || (ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return id != "00000000-0000-0000-0000-000000000000"
}
