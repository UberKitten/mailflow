# Envelope Lookup (missing To: recipients)

When emails arrive via BCC or "undisclosed recipients", the `To:` header is empty. This breaks rules that use `to:` or `to_domain:` matching. Envelope lookup recovers the original SMTP `RCPT TO` address by querying the [Graph Message Trace API](https://learn.microsoft.com/en-us/exchange/monitoring/trace-an-email-message/graph-api-message-trace).

This feature is **optional** and requires certificate-based auth.

## How it works

1. mailflow detects an email with an empty `To:` header
2. it calls the Graph beta message trace API with the sender address and approximate received time
3. the API returns trace entries including the original envelope recipient
4. mailflow injects the resolved address into `msg.To` so rules can match on it

The `mailflow debug` command always attempts lookup when cert auth is configured. Normal processing only runs lookups when `enabled_in_processing` is true.

## Configuration

### 1. Certificate auth (in `graph` section)

Add cert fields to your existing `graph:` config:

```yaml
graph:
  client_id: "your-app-id"
  tenant_id: "your-tenant-id"
  token_file: "/config/.ms-graph-token.json"
  
  # Certificate for app-only auth (optional, enables envelope lookup)
  cert_path: "/config/exo-cert.pfx"
  cert_password_file: "/config/exo-cert-password"   # file containing the password
```

### 2. Envelope lookup behavior (optional)

```yaml
envelope_lookup:
  timeout: 10                    # seconds (default: 10)
  enabled_in_processing: false   # run during daemon mode (default: false)
```

If `envelope_lookup` is omitted entirely, the debug command still performs lookups (using the default 10s timeout) as long as `cert_path` is configured.

## Azure AD setup

The app registration needs:

1. **`ExchangeMessageTrace.Read.All`** — Application permission on Microsoft Graph
2. **Admin consent** granted for the tenant  
3. **Microsoft service principal provisioned** in your tenant:
   ```powershell
   Connect-MgGraph -Scopes "Application.ReadWrite.All"
   New-MgServicePrincipal -BodyParameter @{appId="8bd644d1-64a1-4d4b-ae52-2e0cbf64e373"}
   ```
   (one-time setup, may take hours to propagate)

The same certificate used for Exchange.ManageAsApp works — just add the Graph permission alongside it.

## Retention limits

Message trace data is **not permanent**:
- The Graph API covers up to **90 days** of history
- Older messages will return no results

## Debug command output

When `To:` is empty and cert auth is configured:

```
Email: "Subject" from sender@domain.com
⚠ No To: recipients — envelope lookup: original-alias@yourdomain.com
```

The resolved address is then used for rule matching in the debug output.
