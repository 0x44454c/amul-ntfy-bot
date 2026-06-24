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
    echo "Creating $INSTALL_DIR/.env — edit with your BOT_TOKEN"
    cat > "$INSTALL_DIR/.env" <<'EOF'
BOT_TOKEN=your_bot_token_here
EOF
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
