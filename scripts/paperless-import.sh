#!/bin/bash
# paperless-import.sh - Import PDF attachments from emails to Paperless-ngx
#
# Reads JSON from stdin with email metadata (from mailflow exec action)
# Downloads PDF attachments via MS Graph API and copies to paperless consume folder.

set -euo pipefail

CONSUME_DIR="apps:~/paperless-ngx/consume/"
TEMP_DIR="${TMPDIR:-/tmp}/paperless-import-$$"
LOG_PREFIX="[paperless-import]"

log() {
    echo "$LOG_PREFIX $*" >&2
}

cleanup() {
    if [[ -d "$TEMP_DIR" ]]; then
        rm -rf "$TEMP_DIR"
    fi
}
trap cleanup EXIT

# Read JSON from stdin
INPUT=$(cat)

# Extract message_id (which is actually the Graph API message ID)
MESSAGE_ID=$(echo "$INPUT" | jq -r '.message_id // .id')
SUBJECT=$(echo "$INPUT" | jq -r '.subject')
FROM=$(echo "$INPUT" | jq -r '.from')

if [[ -z "$MESSAGE_ID" || "$MESSAGE_ID" == "null" ]]; then
    log "ERROR: No message_id in input"
    exit 1
fi

log "Processing message: $SUBJECT (from: $FROM)"

# Get Graph API token
TOKEN=$(~/bin/ms-graph-token.sh)
if [[ -z "$TOKEN" ]]; then
    log "ERROR: Failed to get Graph API token"
    exit 1
fi

BASE_URL="https://graph.microsoft.com/v1.0/me/messages"

# List attachments
ATTACHMENTS=$(curl -s -H "Authorization: Bearer $TOKEN" \
    "$BASE_URL/$MESSAGE_ID/attachments" | jq -c '.value // []')

ATTACHMENT_COUNT=$(echo "$ATTACHMENTS" | jq 'length')
log "Found $ATTACHMENT_COUNT attachment(s)"

if [[ "$ATTACHMENT_COUNT" -eq 0 ]]; then
    log "No attachments to import"
    exit 0
fi

mkdir -p "$TEMP_DIR"
IMPORTED=0

# Process each attachment
echo "$ATTACHMENTS" | jq -c '.[]' | while read -r attachment; do
    NAME=$(echo "$attachment" | jq -r '.name')
    CONTENT_TYPE=$(echo "$attachment" | jq -r '.contentType // ""')
    ATTACHMENT_ID=$(echo "$attachment" | jq -r '.id')
    
    # Only process PDFs
    if [[ "$CONTENT_TYPE" != "application/pdf" && ! "$NAME" =~ \.pdf$ ]]; then
        log "Skipping non-PDF: $NAME ($CONTENT_TYPE)"
        continue
    fi
    
    log "Downloading: $NAME"
    
    # Check if it's a file attachment with inline content
    CONTENT_BYTES=$(echo "$attachment" | jq -r '.contentBytes // ""')
    
    if [[ -n "$CONTENT_BYTES" && "$CONTENT_BYTES" != "null" ]]; then
        # Content is inline in base64
        echo "$CONTENT_BYTES" | base64 -d > "$TEMP_DIR/$NAME"
    else
        # Need to fetch content separately
        curl -s -H "Authorization: Bearer $TOKEN" \
            "$BASE_URL/$MESSAGE_ID/attachments/$ATTACHMENT_ID/\$value" \
            -o "$TEMP_DIR/$NAME"
    fi
    
    if [[ -f "$TEMP_DIR/$NAME" && -s "$TEMP_DIR/$NAME" ]]; then
        log "Copying to paperless: $NAME"
        scp "$TEMP_DIR/$NAME" "$CONSUME_DIR"
        IMPORTED=$((IMPORTED + 1))
        log "Imported: $NAME"
    else
        log "ERROR: Failed to download $NAME"
    fi
done

log "Import complete: $IMPORTED PDF(s) imported"
