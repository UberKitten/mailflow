# mailflow — Agent Skill

Email sorting daemon for Microsoft 365 using Graph API. Rules are YAML files evaluated in priority order.

## Architecture

- **Config repo** (private): `rules.d/*.yaml`, `senders.d/*.yaml`, `config.yaml`
- **Code repo** (public): Go source, Dockerfile, CI
- **Runtime**: Docker container, webhook + polling daemon
- **Hot reload**: `SIGHUP` reloads config without restart

## Common tasks

### Debug why an email went to the wrong folder
```bash
# On the server (or via docker compose run):
mailflow debug '<internet-message-id>'
# Shows every rule evaluated, which matched, and why
```

### Move a misrouted email
```bash
mailflow debug '<internet-message-id>' move
```

### Add a new sorting rule

1. Identify the sender's `from` address or domain
2. Choose the right rule file by priority (lower number = higher priority):
   - `08-address-overrides.yaml` — specific addresses that override domain rules
   - `10-security.yaml` — security/login alerts
   - `15-alerts.yaml` — service alerts, monitoring
   - `40-posts-subfolders.yaml` — newsletters/blogs by subfolder
   - `45-posts.yaml` — catch-all for newsletter domains
   - `50-promotions.yaml` — marketing emails
   - `99-catchall.yaml` — catch-all (everything unmatched)
3. Add the rule:
   ```yaml
   - name: descriptive-name    # must be unique across all files
     folder: Inbox/TargetFolder
     from:                      # or from_domain for all addresses at a domain
       - sender@example.com
   ```
4. Run `mailflow validate` to check for errors
5. Commit, push, deploy

### Find unmatched emails (gap analysis)
```bash
mailflow gaps Inbox --recursive --fast    # fast skips body-based rules
mailflow gaps Inbox/Unsorted              # check unsorted folder
```

### Resort existing emails after rule changes
```bash
mailflow resort Inbox/Security --dry-run   # preview
mailflow resort Inbox/Security             # apply
mailflow resort-sender Inbox '*@github.com' --dry-run  # by sender
```

### Deploy rule changes
```bash
# Config-only (hot reload, no container restart):
# 1. git commit + push in config repo
# 2. git pull on server
# 3. docker exec mailflow /app/mailflow --config-dir=/config reload

# Code changes (rebuild needed):
# docker compose build && docker compose up -d
```

### Validate config
```bash
mailflow validate                # check for errors and warnings
mailflow validate --strict       # treat warnings as errors
mailflow validate --diff         # focus on recently changed rules
```

## Rule matching

Rules are evaluated in filename order. First match wins. Available matchers:

| Field | Matches |
|-------|---------|
| `from` | Exact sender email address |
| `from_domain` | Sender's email domain (supports `!ref` sender lists) |
| `to` / `to_domain` | Recipient address/domain |
| `subject_contains` | Substring match on subject |
| `from_name` / `from_name_contains` | Sender display name |
| `header_contains` | Any email header value |
| `catchall: true` | Matches everything |

## Important patterns

- **Address overrides**: When a domain is in a broad rule (e.g., `github.com` in security), use a higher-priority rule for specific addresses (`no-reply@github.com` → Promotions)
- **Sender lists**: Define domains/addresses in `senders.d/*.yaml`, reference with `!ref listname` to avoid repetition
- **Catch-all → Unsorted**: The last rule should be a catch-all routing to Unsorted. Emails there = missing rules.

## Gotchas

- **Graph API message IDs change on move** — don't cache IDs across move operations
- **`docker compose up -d` doesn't restart on config-only changes** — use `reload` (SIGHUP) instead
- **Duplicate rule names cause validation errors** — names must be unique across all files
