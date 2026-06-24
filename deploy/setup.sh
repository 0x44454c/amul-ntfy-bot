#!/bin/bash
set -e

echo "=== ntfy-bot Linux deployment ==="

INSTALL_DIR=/opt/ntfy-bot
SERVICE_NAME=ntfy-bot

# Create install directory
sudo mkdir -p "$INSTALL_DIR"

# Copy binary
sudo cp "$(dirname "$0")/../ntfy_bot" "$INSTALL_DIR/"

# Create .env if missing
if [ ! -f "$INSTALL_DIR/.env" ]; then
    echo "Creating $INSTALL_DIR/.env — edit with your BOT_TOKEN"
    sudo bash -c "cat > $INSTALL_DIR/.env" <<'EOF'
BOT_TOKEN=your_bot_token_here
EOF
fi

# Install service
sudo cp "$(dirname "$0")/ntfy-bot.service" /etc/systemd/system/$SERVICE_NAME.service
sudo systemctl daemon-reload
sudo systemctl enable $SERVICE_NAME
sudo systemctl restart $SERVICE_NAME

echo "=== Done ==="
sudo systemctl status $SERVICE_NAME --no-pager
