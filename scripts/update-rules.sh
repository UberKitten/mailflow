#!/bin/bash
# Update mailflow rules from git on apps server
# Run from desktop: ssh apps "~/mailflow/scripts/update-rules.sh"
# Or locally on apps: ~/mailflow/scripts/update-rules.sh

set -e

MAILFLOW_DIR="${MAILFLOW_DIR:-$HOME/mailflow}"
CONFIG_DIR="${CONFIG_DIR:-$HOME/.config/appdata/mailflow}"

cd "$MAILFLOW_DIR"

echo "==> Pulling latest from git..."
git pull

echo "==> Syncing rules.d..."
cp -r config/rules.d/* "$CONFIG_DIR/rules.d/"

echo "==> Syncing senders.d..."
cp -r config/senders.d/* "$CONFIG_DIR/senders.d/"

echo "==> Reloading mailflow config..."
docker kill -s HUP mailflow || echo "Warning: Could not send SIGHUP (container may not be running)"

echo "==> Done! Recent logs:"
docker logs mailflow --tail 5 2>&1 || true
