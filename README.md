# ntfy-bot

Amul protein stock notifier Telegram bot, ported from [amul-notify](https://github.com/SwapnilSoni1999/amul-notify) by SwapnilSoni1999 from TypeScript to Go.

## Features

- Track Amul protein product availability by pincode
- Notify via Telegram when tracked products come back in stock
- Two tracking modes: **Notify** (limited notifications, auto-remove) and **Always** (notify per stock cycle)
- Favourite products for quick access
- SQLite database (single binary, no external deps)

## Build

```bash
# Build for development (DB: ./amul.db)
go build -o ntfy_bot .

# Build for production (DB: ~/ntfy-bot/data/amul.db)
go build -tags prod -ldflags="-s -w" -o ntfy_bot .

# Cross-compile for linux
GOOS=linux GOARCH=amd64 go build -tags prod -ldflags="-s -w" -o ntfy_bot .
```

## Deploy (user service)

**First time:**

```bash
./deploy/setup.sh
```

**Redeploy** (build & restart only):

```bash
./deploy/setup.sh --redeploy
```

Or manually:

```bash
mkdir -p ~/ntfy-bot
cp ntfy_bot ~/ntfy-bot/
cp .env.example ~/ntfy-bot/.env  # or your own .env with BOT_TOKEN
mkdir -p ~/.config/systemd/user
cp deploy/ntfy-bot.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now ntfy-bot
loginctl enable-linger  # keep service running after logout
```

## Usage

| Command | Description |
|---|---|
| `/start` | Welcome message |
| `/setpincode <pincode>` | Set your delivery pincode |
| `/products` | List available protein products |
| `/tracked` | Show your tracked products |
| `/favourites` | Show favourite products |
| `/settings` | Change tracking mode or notification count |

## Tech

- **Language:** Go
- **Database:** SQLite via GORM
- **Bot Framework:** go-telegram/bot
- **Single binary deployment**

## Credits

Inspired by [amul-notify](https://github.com/SwapnilSoni1999/amul-notify) by [SwapnilSoni1999](https://github.com/SwapnilSoni1999).
