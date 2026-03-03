# mailflow

Email sorting and notification daemon for Microsoft 365 / Outlook, powered by the Graph API.

Mailflow watches your mailbox via webhook notifications, evaluates emails against YAML-based rules, and moves them to the right folders automatically. It also sends push notifications (via Pushover) for things like verification codes and fraud alerts.

## Why?

Outlook's built-in rules are limited: no regex, no domain matching, no priority ordering, no reusable sender lists. Server-side rules can't be version controlled or shared across accounts.

Mailflow replaces all of that with a simple, git-trackable YAML config.

## The catch-all → unsorted pattern

The recommended approach: write rules for every known sender, and use a **catch-all rule** to route everything else to an `Unsorted` folder:

```yaml
# 10-security.yaml
rules:
  - name: security-senders
    folder: Inbox/Security
    from_domain:
      - github.com
      - accounts.google.com

# 99-catchall.yaml
rules:
  - name: catchall-to-unsorted
    folder: Unsorted
    catchall: true
```

When a new sender appears, it lands in Unsorted — making it obvious you need a new rule. Over time, Unsorted trends toward zero. This turns your inbox from a firehose into a curated system where every email has a home.

## Features

- **Priority-based rules** — files are evaluated in filename order (`08-overrides.yaml` beats `10-security.yaml`)
- **Reusable sender lists** — define domain/address lists once, reference them in rules with `!ref`
- **Rich matching** — `from`, `from_domain`, `to`, `to_domain`, `subject_contains`, `header_contains`, `from_name`, `from_name_contains`, catch-all
- **Folder categories** — auto-apply Outlook categories based on destination folder
- **Pushover notifications** — send alerts for verification codes, fraud warnings, etc.
- **Webhook + polling** — real-time via Graph API change notifications, with polling fallback
- **Hot reload** — `SIGHUP` reloads config without restarting the daemon
- **Validation** — `mailflow validate` catches duplicate rule names, dangerous overlaps, and config errors before deploy
- **Debug tools** — `mailflow debug <message-id>` shows exactly which rules match and why
- **Gap analysis** — `mailflow gaps` finds emails that don't match any rule
- **Bulk resort** — `mailflow resort` re-evaluates and moves existing mail
- **Actions** — trigger scripts on match (e.g., import PDF attachments to Paperless-ngx)

## Quick start

### 1. Get a Graph API token

Register an app in Azure AD with `Mail.ReadWrite` permissions and obtain a refresh token. See [Microsoft's guide](https://learn.microsoft.com/en-us/graph/auth-v2-user).

Create a token script that outputs a valid access token on stdout:

```bash
#!/bin/bash
# your-token-script.sh — refresh and output a Graph API access token
# mailflow calls this whenever it needs a token
```

### 2. Configure

```bash
cp config/config.yaml.example config/config.yaml
# Edit with your token script path and Pushover credentials (optional)
```

### 3. Write rules

```bash
mkdir -p config/rules.d config/senders.d
# See config/rules.d.example/ and config/senders.d.example/ for templates
```

Rules are YAML files in `config/rules.d/`, evaluated in filename order. Lower numbers = higher priority.

### 4. Run

```bash
# Docker (recommended)
docker compose up -d

# Or run directly
go build -o mailflow ./cmd/mailflow
./mailflow --config-dir=./config webhook
```

## Rule format

```yaml
version: 1
rules:
  - name: my-rule              # unique name (required)
    folder: Inbox/MyFolder     # destination folder (required)
    from:                      # match sender address (exact)
      - alerts@example.com
    from_domain:               # match sender domain
      - example.com
    from_domain: !ref security # or reference a sender list
    to:                        # match recipient address
      - me@example.com
    to_domain:                 # match recipient domain
      - example.com
    subject_contains:          # match subject substring
      - "verification code"
    from_name_contains:        # match sender display name
      - "Security Alert"
    header_contains:           # match any header value
      - "List-Id: <mylist.example.com>"
    catchall: true             # matches everything (for catch-all rules)
    categories:                # Outlook categories to apply
      - "Important"
    on_match:                  # actions on match
      pushover:
        title: "Alert"
        priority: 1
      exec:                    # run a script, email JSON on stdin
        command: /app/scripts/my-script.sh
```

### Sender lists (`senders.d/`)

```yaml
name: security
domains:
  - github.com
  - accounts.google.com
addresses:
  - security@custom-domain.com
```

Reference in rules: `from_domain: !ref security`

### Folder categories

Auto-apply Outlook categories based on destination folder:

```yaml
# config.yaml
folder_categories:
  - folder: "Inbox/Posts"
    categories: ["Post"]
  - folder: "Inbox/Security"
    categories: ["Security"]
```

Categories are merged with any rule-level `categories`.

## Commands

```bash
# Daemon mode — webhook + polling
mailflow webhook

# Process recent mail (one-shot)
mailflow process --since=1h

# Resort existing mail in a folder
mailflow resort Inbox/Security
mailflow resort Inbox --recursive

# Resort by sender across folders
mailflow resort-sender Inbox/Security '*@github.com' --dry-run

# Find emails that don't match any rule
mailflow gaps Inbox --recursive --fast

# Debug a specific email
mailflow debug '<message-id>'
mailflow debug '<message-id>' move    # debug + move to correct folder

# Apply non-move actions (categories, exec) to existing mail
mailflow apply-actions Inbox/Posts --recursive

# Validate config
mailflow validate
mailflow validate --strict            # warnings become errors
mailflow validate --diff              # extra scrutiny on recently changed rules

# Reload config (hot reload via SIGHUP)
mailflow reload

# Check config syntax
mailflow config check
```

## Docker

```bash
# Basic
docker compose up -d

# With custom config location
MAILFLOW_CONFIG_DIR=/path/to/config docker compose up -d

# Rebuild after code changes
docker compose build && docker compose up -d

# Hot-reload after config changes (no restart needed)
docker exec mailflow /app/mailflow --config-dir=/config reload
```

See `docker-compose.override.yml.example` for reverse proxy and volume mount examples.

## Deployment workflow

The recommended flow for rule changes:

1. Edit rules locally in your config repo
2. Run `mailflow validate` to check for errors
3. Commit and push
4. On the server: `git pull` → `docker exec mailflow ... reload`

Keeping config in a separate (private) repo lets you version-control your rules without exposing your sender lists publicly.

## License

MIT
