# mailflow

Email sorting and notification daemon using Microsoft Graph API.

## Features

- Rule-based email sorting with YAML config
- Pushover notifications for verification codes and fraud alerts
- Webhook endpoint for real-time Graph API notifications
- Bulk resort and gap analysis commands

## Commands

```bash
# Process new mail (daemon mode)
mailflow process --watch

# Process recent mail
mailflow process --since=1h

# Resort existing mail (no notifications)
mailflow resort [folder]

# Find emails that don't match any rule
mailflow gaps

# Validate config
mailflow config check

# Reload config (send SIGHUP)
mailflow reload
```

## Config

Config lives in `/config` (docker) or `~/.config/appdata/mailflow/` (local):

```
config.yaml          # Main config (auth, pushover, settings)
rules.d/*.yaml       # Sorting rules by priority
senders.d/*.yaml     # Reusable sender lists
```

## Docker

```bash
docker compose up -d
```

Exposes webhook on port 8792 for Graph API subscriptions.
