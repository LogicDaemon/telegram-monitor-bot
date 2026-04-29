# Host Monitor Bot

A simple Go-based Telegram bot to monitor host connectivity. It sends a message to a specific Telegram channel when the application starts (or re-establishes connection) and updates the message every minute to confirm the host is online. If the connection fails, it will attempt to send a "connection lost" message.

## Features
- Notifies when connection is established.
- Updates the original connection message every minute to show the current time ("Связь есть ...").
- In case of a network or delivery failure, repeatedly attempts to send a "connection lost" message until it succeeds.
- Installed as a systemd service using the provided `deploy.sh`.

## Requirements
- Go 1.24.1 or higher
- A Telegram Bot Token from [@BotFather](https://t.me/botfather).
- A target Telegram Channel / Group with the bot added as an Administrator.

## Configuration

The bot uses two configuration JSON files, one for secrets and one for settings. The application will print instructions with the expected paths if they are not found.

### 1. Secrets file (`telegram_monitor_bot_secrets.json`)

By default on Linux, this is located at `~/.local/_sec/telegram_monitor_bot_secrets.json`. You can also configure the directory using the `SECRETS_PATH` or `SecretDataDir` environment variables.

```json
{
  "telegram_bot_token": "YOUR_TELEGRAM_BOT_TOKEN"
}
```

### 2. Settings file (`settings.json`)

By default on Linux, this is located in `~/.local/telegram-monitor-bot/settings.json`.

```json
{
  "telegram_channel_id": -1000000000000
}
```

*Note: You can get the Telegram Channel ID by forwarding a message from the channel to `@userinfobot`. Be sure to include the `-` or `-100` prefix.*

## Installation & Deployment

A script to build and deploy the bot as a systemd service on Linux is provided.

```bash
# Provide the SECRETS_PATH or other env variables expected in your templated service if needed
./deploy.sh
```

### What `deploy.sh` does:
1. Builds the Go binary (`go build`).
2. Moves the binary to `/usr/local/bin/telegram-monitor`.
3. Interpolates environment variables into `telegram-monitor.service.template` and creates the systemd service.
4. Reloads systemd daemon, enables, and starts `telegram-monitor.service`.

## License
Refer to the `LICENSE` file in the repository.
