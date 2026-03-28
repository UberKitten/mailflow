# Envelope Lookup (missing To: recipients)

Mailflow normally matches rules against the `To:` header. Some emails are delivered via BCC or “undisclosed recipients,” which means the `To:` header is empty. In those cases, mailflow can optionally look up the **envelope recipient** (the SMTP `RCPT TO` address) using an external source and inject it into `msg.To` for matching.

This feature is **optional** and disabled by default.

## Why this exists

When a message is delivered via BCC, Outlook/Graph often returns **no `To:` recipients**. That breaks any rules that rely on `to:` or `to_domain:` matching. Envelope lookup lets mailflow recover the real delivery target so rules still work.

## Configuration

```yaml
envelope_lookup:
  # Option A: shell script (for local/debug use)
  script: "/path/to/envelope-lookup.sh"

  # Option B: HTTP endpoint (for production Docker use)
  url: "http://hostname:port/envelope-lookup"

  # Timeout in seconds (default 10)
  timeout: 10

  # Whether to run during normal processing (default: false)
  enabled_in_processing: false
```

Notes:
- If **both** `script` and `url` are set, the **script** is used.
- If neither is set, envelope lookup is disabled.
- `enabled_in_processing` controls normal processing. The `mailflow debug` command can still run lookups even when this is false.

## Exchange Online lookup script

The provided reference script uses Exchange Online message trace to find the envelope recipient. It takes three positional arguments:

```
./scripts/envelope-lookup-exchange.ps1 "<message-id>" "sender@domain.com" "2026-03-27T01:23:45Z"
```

Setup steps:
1. Install the **ExchangeOnlineManagement** module on the host running the script.
2. Create an Azure AD app with **Exchange.ManageAsApp** (application) permissions.
3. Upload a certificate and grant the app the **Mail Recipients** / **Message Trace** roles.
4. Run the script with app + cert parameters (see script header comments).

The script outputs **only** the recipient address on stdout. Errors are written to stderr.

## Future path: Graph Message Trace API

Microsoft’s Graph Message Trace API is the long-term replacement for PowerShell-based traces. Once it’s available and reliable for this use-case, the HTTP option should switch to a Graph-backed lookup service instead of Exchange PowerShell.

## Retention limits (important)

Message trace data is **not permanent**:
- Standard message trace queries typically cover **~10 days** of history.
- Historical message trace extends up to **~90 days**, but requires a different workflow.

If a message is older than those limits, envelope lookup will fail.
