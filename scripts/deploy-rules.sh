#!/bin/bash
# Deploy mailflow rules from desktop to apps
# Usage: ./scripts/deploy-rules.sh [commit message]

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
MAILFLOW_DIR="$(dirname "$SCRIPT_DIR")"

cd "$MAILFLOW_DIR"

# Sync local config to repo
echo "==> Syncing local config to repo..."
LOCAL_CONFIG="${LOCAL_CONFIG:-$HOME/.config/appdata/mailflow}"
cp -r "$LOCAL_CONFIG/rules.d/"* config/rules.d/
cp -r "$LOCAL_CONFIG/senders.d/"* config/senders.d/

# Check for changes
if git diff --quiet config/; then
    echo "No changes to deploy."
    exit 0
fi

# Show what changed
echo "==> Changes:"
git diff --stat config/

# Commit and push
MSG="${1:-Update mailflow rules}"
echo "==> Committing: $MSG"
git add config/
git commit -m "$MSG"
git push

# Update apps
echo "==> Updating apps server..."
SSH_AUTH_SOCK="${SSH_AUTH_SOCK:-/home/m/.ssh/agent.sock}" ssh apps "~/mailflow/scripts/update-rules.sh"

echo "==> Deploy complete!"
