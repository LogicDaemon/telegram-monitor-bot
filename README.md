# Host Monitor Bot

A Go service that monitors host connectivity. Telegram gets `Наблюдение начато <RFC1123>` at startup and `Был сбой, переподключение <RFC1123>` after an update failure; the message is edited every minute while updates succeed. WhatsApp answers status queries only: a participant who sends the number text matching `status_keywords` gets `Связь есть <RFC1123>` back in that 1:1 thread.

When WhatsApp and the webhook are configured, the service receives Cloud API events, answers matching queries, and logs delivery statuses.

## Features
- Sends "Наблюдение начато ..." to the Telegram channel at start, and "Был сбой, переподключение ..." after an update failure.
- Updates that message every minute to show the current time ("Связь есть ..."). The frozen message keeps the time of its last successful update, so the gap between it and the next start or reconnect message shows the outage window.
- A participant who sends text matching `status_keywords` to the number gets `Связь есть <RFC1123>` answered in the same thread. Inbound message IDs are deduplicated in process; a retry of an uncertain send can still send the answer twice.
- Delivery statuses (`sent`, `delivered`, `read`, `failed`) are logged once per message-status-recipient transition, with each reported error code and message. Nothing acts on statuses: replies are retried from send responses, not status webhooks.
- Uploads the current status to `app:/status.txt` on Yandex Disk every 10 minutes.
- Installed as a systemd service using the provided `deploy.sh`.

## Requirements
- Go 1.24.1 or higher
- A Telegram Bot Token from [@BotFather](https://t.me/botfather).
- A target Telegram Channel / Group with the bot added as an Administrator.
- For WhatsApp answering (optional): a Cloud API number, its system user access token and phone number ID, and a payment method on file for the Messaging account. No Official Business Account is needed, because replies are 1:1 service messages inside the customer service window each participant opens. The publisher is disabled unless the token and the phone number ID are both configured.
- For the webhook (optional): the Meta app secret, a verify token, a public HTTPS Callback URL, and a listen address. A Cloudflare Tunnel on a domain you manage exposes a host with a dynamic IP.
- Optional: a static Yandex Disk API application token with the `cloud_api:disk.app_folder` permission.

## Configuration

The bot uses two configuration JSON files, one for secrets and one for settings. The application will print instructions with the expected paths if they are not found.

### 1. Secrets file (`host_monitor_bot_secrets.json`)

By default on Linux, this is located at `~/.local/_sec/host_monitor_bot_secrets.json`. You can also configure the directory using the `SECRETS_PATH` or `SecretDataDir` environment variables.

```json
{
  "telegram_bot_token": "YOUR_TELEGRAM_BOT_TOKEN",
  "whatsapp_access_token": "YOUR_WHATSAPP_ACCESS_TOKEN",
  "whatsapp_phone_number_id": "YOUR_WHATSAPP_PHONE_NUMBER_ID",
  "whatsapp_app_secret": "YOUR_WHATSAPP_APP_SECRET",
  "webhook_verify_token": "YOUR_WEBHOOK_VERIFY_TOKEN",
  "yandex_disk_client_token": "YOUR_YANDEX_DISK_CLIENT_TOKEN"
}
```

`yandex_disk_client_token` is optional. If it is missing, the Telegram monitor starts without the Yandex Disk worker.

`whatsapp_access_token` and `whatsapp_phone_number_id` are optional. The publisher is created only when both are non-empty; if one is set without the other, the service logs that WhatsApp is partially configured. Both come from the Meta developer app (WhatsApp > API Setup): a system user access token holding `whatsapp_business_messaging`, and the phone number ID of the Cloud API number. 1:1 answering needs no Official Business Account and no Business Verification — see [WhatsApp answering](#whatsapp-answering) for the account-side conditions that do apply.

`whatsapp_app_secret`, `webhook_verify_token`, and `webhook_listen` are required together to start the webhook, and a WhatsApp publisher must exist. The app secret is the Meta app's App Secret (App Dashboard > Settings > Basic), used for the `X-Hub-Signature-256` HMAC; the verify token must match the Verify token field of the webhook configuration. With a publisher, any missing webhook field logs a warning listing every missing field; the webhook stays disabled until all three are set.

The static Yandex Disk API application token is obtained using this link:

<https://oauth.yandex.ru/authorize?response_type=token&client_id=ca79e936dc1646998180d04b87e678b6>

The API application must have the Yandex Disk permission `cloud_api:disk.app_folder`. This application does not perform an OAuth exchange at runtime; it sends the stored token as `Authorization: OAuth <token>` when requesting the upload URL.

### 2. Settings file (`settings.json`)

By default on Linux, this is located in `~/.local/telegram-monitor-bot/settings.json`.

```json
{
  "telegram_channel_id": -1000000000000,
  "webhook_listen": "127.0.0.1:8080",
  "status_keywords": "(?i)^(статус|status)$"
}
```

*Note: You can get the Telegram Channel ID by forwarding a message from the channel to `@userinfobot`. Be sure to include the `-` or `-100` prefix.*

*Note: `webhook_listen` is the plain HTTP address the webhook server binds to, such as `127.0.0.1:8080`. TLS is terminated in front of it, see [Webhook (Cloudflare Tunnel)](#webhook-cloudflare-tunnel). It is used only when a WhatsApp publisher exists and both webhook secrets and this listen address are configured.*

*Note: `status_keywords` is optional and is a single Go regular-expression string matched against text messages. An omitted or empty value matches every text message. Use Go regular-expression syntax to define the desired matching behavior.*

## WhatsApp answering

- 1:1 only. A participant sends the number text matching `status_keywords` and gets `Связь есть <RFC1123>` back in that thread. Nothing is announced about connection events; the Telegram start message is sent by `main`. Inbound messages carrying a `group_id` are ignored, so group conversations are not answered. No Official Business Account is required: the Groups API is what needs an OBA, and this service does not use it.
- The reply is addressed to the participant who asked: `to` with the phone number from `from`, or `recipient` with the business-scoped user id from `from_user_id` when Meta omitted the number because the participant has adopted a username. A query carrying neither identifier is logged and not answered.
- `runWhatsappResponder` (`whatsapp-monitor.go`) runs as one goroutine. It returns for a nil server; otherwise it binds the listener, starts serving in a nested goroutine, then ticks at `RetryDelay` (one minute) and re-sends any queued answer. A bind failure is logged and returns. `main` starts it only when a publisher exists; it keeps no WhatsApp state and has no channel to the responder.
- `publishReply(reply)` returns `(attempted, retryable, err)`. During the pacing window it queues the answer together with its recipient and returns `(false, true, nil)`; success or a terminal failure clears the queue, while a retryable failure keeps it queued. A newer query replaces a queued answer, including its recipient, so an earlier query from another participant is dropped rather than answered. `takePendingReply` reads under the lock, and the handler and responder use `reportReplyOutcome` for the same log wording.
- `permanentSendStatus` treats 4xx responses as terminal except 429 and 408. `transientSendCodes` (`130429`, `131000`, `131016`, `131056`, `131057`, `133004`) makes a listed Graph API envelope code retryable even for an otherwise-terminal status; this classification is used only on the synchronous send response. A 429 sets pacing from `Retry-After` seconds, capped at one hour (`maxPacing`); an invalid, missing, or non-positive value falls back to `RetryDelay`.
- Delivery is at-least-once: transport failures, retryable HTTP responses, and a 200 response that does not parse, has no messages, or has no message ID leave the answer eligible for retry. Meta may have accepted a send whose response was lost or invalid, so a retry can send the answer twice. Status webhooks do not trigger retries.
- The handler deduplicates inbound message IDs in its in-memory state, so duplicate IDs in a batch or a redelivery within the process are processed once. A queued send retry can still produce another reply.
- `sent` / `delivered` / `read` / `failed` status transitions are logged once per message-status-recipient transition, with each error code and message. Nothing acts on statuses; replies are retried from send responses, not status webhooks.
- Meta permits free-form (service) text only inside the 24-hour customer service window, which opens and resets with each message a participant sends; `131047` means the window has passed and only a new message from that participant reopens it, so it is not retried. The query that triggers a reply opens the window the reply goes out in. No template send path is implemented or needed, because this service does not send outside a window.
- The Graph API version is pinned to `v27.0` in `WhatsAppGraphBaseURL` (`whatsapp-monitor.go`).
- Account-side conditions the code cannot clear, since 2026-10-01: service messages are charged per delivered recipient and are free only while the number is inside its 1,000-message monthly service tier. Meta does not deliver service messages at all for a Messaging account with no payment method on file, which it required by 2026-09-30; that appears as a `131042` rejection, which the retry loop does not repeat because billing corrections are not a waiting period. Replies inside an open window do not consume the portfolio's 250-recipient-per-24-hour messaging limit, which counts only deliveries outside windows.

## Webhook (Cloudflare Tunnel)

The webhook server starts only when a WhatsApp publisher exists and `whatsapp_app_secret`, `webhook_verify_token`, and `webhook_listen` are configured. With no publisher, the responder is not started. With a publisher and any missing webhook field, a warning lists every missing field and the webhook stays disabled. The responder returns after logging a bind failure; otherwise it starts serving in a nested goroutine beside its retry ticker, and logs a serving failure.

Meta requires a valid public HTTPS certificate at the registered Callback URL and rejects self-signed certificates; no port restriction is documented, so the port only has to appear in the URL you register (`https://host:8443/webhook`). The server here listens on plain HTTP, so TLS is terminated in front of it. A Cloudflare named tunnel on a domain you manage is the setup that works with a dynamic IP, because `cloudflared` connects outward and needs no port forwarding:

```bash
cloudflared tunnel create host-monitor
cloudflared tunnel route dns host-monitor wa.4q.fyi

# ~/.cloudflared/config.yml
# tunnel: <tunnel-id>.json
# ingress:
#   - hostname: wa.4q.fyi
#     service: http://127.0.0.1:8080
#   - service: http://localhost:40441   # catch-all, for cloudflared's own diagnostics
```

Then the Callback URL is `https://wa.4q.fyi/webhook` and `webhook_listen` is `127.0.0.1:8080`. Cloudflare Access in front of that path must be off, otherwise Meta's requests get an auth challenge. Quick tunnels (`cloudflared tunnel --url http://localhost:8080`, hostnames under `*.try.cloudflare.net`) change on every restart, so the Callback URL verification breaks whenever the tunnel restarts: they are not usable for a webhook configuration.

Account setup (WhatsApp):

- The number must be a Cloud API number, not a WhatsApp Business app number on the same SIM.
- Add a payment method for the Messaging account in WhatsApp Manager. Without one, Meta does not deliver service messages, which is what a reply is.
- While the app is in Development mode, a send reaches only the phone numbers registered as testers for the app. Add the participants' numbers there, or switch the app to Live; the tester panel and the mode toggle are both in the App Dashboard.
- Business Verification and an Official Business Account are not needed for 1:1 answering. They are the prerequisites for the Groups API, which this service does not use.

App Dashboard (WhatsApp > Configuration):

- Callback URL: `https://<your-hostname>/webhook`
- Verify token: the same string as `webhook_verify_token`
- Subscribed fields: `messages`. It carries inbound 1:1 messages and delivery statuses, which are all this service acts on. Other fields an app can subscribe to are acknowledged with `200` and dropped, so subscribing to them costs nothing but adds nothing.
- Permission `whatsapp_business_messaging`
- The app must be in **Live** mode, otherwise some webhooks are not sent

The endpoint handles:

- **GET** answers the `hub.challenge` verification handshake, which Meta sends every time the Callback URL or Verify token is set.
- **POST** verifies `X-Hub-Signature-256` — the HMAC-SHA256 of the raw body keyed by `whatsapp_app_secret`, compared in constant time — and reads at most Meta's documented 3 MB payload: a body above the ceiling is answered `413`, not processed as a truncated prefix. The `200` acknowledgement is written and flushed before any processing, because writing the header alone leaves it in the connection buffer until the handler returns, and Meta retries an event that is not acknowledged.
- Payloads whose `messaging_product` is not `whatsapp`, or whose `phone_number_id` is not the configured number, are ignored: a business account can hold several numbers, and the webhook subscription covers all of them.
- `sent` / `delivered` / `read` / `failed` statuses are logged once per message-status-recipient transition. The recipient is named by `recipient_user_id` (the business-scoped user id), falling back to `recipient_id` (the phone number): Meta omits `recipient_id` when the reply went to a BSUID and the participant has adopted a username, and omits `recipient_user_id` on a `failed` status reported for a reply sent to a phone number, so both identifiers are part of the deduplication key and the log names the message alone when neither arrives. The codes of a `failed` status are read as raw JSON, because `json.Unmarshal` rejects the whole payload when a field's type does not match the shape the code arrives in, and the `200` acknowledgement has already been sent by then, which drops the delivery instead of causing a retry. The code and the message of every reported error are logged, and nothing acts on them.
- A participant who sends text matching `status_keywords` to the number gets `Связь есть <RFC1123>` answered in that thread, subject to the pacing gate. The message ID is recorded for in-process deduplication; retries of an uncertain send can still produce a second reply. Inbound messages carrying a `group_id` are ignored. A queued answer is re-sent by the responder's ticker.
The HTTP server bounds connections: header, read, write and idle timeouts are set, so a slow client cannot hold a connection and a handler goroutine. The body cap bounds the payload size; the timeouts bound the time. `WriteTimeout` starts when the request headers are read, so it is set above the read budget: a body that takes most of the 30 second read budget still leaves time to write the acknowledgement, which is written after the body is read.

Meta retries a webhook only when the endpoint did not answer `200`, with decreasing frequency for up to 7 days, and the retries sent to all subscribed apps can produce duplicate notifications. Inbound message ids, and the message-status-recipient transitions the handler logged, are deduplicated against two in-memory maps keyed by the handler's own observation time, and entries older than the 7-day retry horizon are dropped whenever a new entry is written, so the maps cannot grow without a bound. Meta's trigger time arrives under `timestamp` as a Unix timestamp string and is not parsed: a `time.Time` field named `timestamp` makes `json.Unmarshal` reject the payload, and the handler has already acknowledged the delivery with `200`, so Meta does not retry it. The state is not persisted, and the argument is the acknowledgement order: the handler writes `200` before it processes the payload, so a delivery that arrives in a new process is one the previous process never acknowledged, and the entry a persisted set would need was never written. A persisted set of answered ids therefore cannot deduplicate any delivery, and the in-memory set covers the duplicates Meta does send: batched payloads carrying the same message twice, and retried deliveries arriving inside the lifetime of one process. The accepted consequence is a second reply, and a status log line repeated after a restart, when a delivery the handler never acknowledged arrives after the restart.

Mutual TLS is supported by Meta (client certificate signed by `meta-outbound-api-ca-2025-12.pem`, common name `client.webhooks.fbclientcerts.com`) and is the supported alternative to IP allowlists; it is not used here, because the Cloudflare Tunnel gives a stable hostname and TLS at the edge.

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
