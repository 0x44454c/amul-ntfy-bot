#!/bin/bash
set -e

echo "=== ntfy-bot user service deployment ==="

INSTALL_DIR="$HOME/ntfy-bot"
SERVICE_NAME=ntfy-bot

# Create directories
mkdir -p "$INSTALL_DIR"

# Build binary with prod tag
CGO_ENABLED=1 go build -tags prod -ldflags="-s -w" -o "$INSTALL_DIR/ntfy_bot" .

# Create .env if missing
if [ ! -f "$INSTALL_DIR/.env" ]; then
    if [ -f "$(dirname "$0")/../.env" ]; then
        cp "$(dirname "$0")/../.env" "$INSTALL_DIR/"
    else
        cp "$(dirname "$0")/../.env.example" "$INSTALL_DIR/.env"
        echo "Created $INSTALL_DIR/.env from .env.example — edit with your BOT_TOKEN"
    fi
fi

# Install user service
mkdir -p ~/.config/systemd/user
cp "$(dirname "$0")/ntfy-bot.service" ~/.config/systemd/user/$SERVICE_NAME.service
systemctl --user daemon-reload
systemctl --user enable $SERVICE_NAME
systemctl --user restart $SERVICE_NAME

# Enable lingering so service starts on boot without login
loginctl enable-linger

echo "=== Done ==="
systemctl --user status $SERVICE_NAME --no-pager
