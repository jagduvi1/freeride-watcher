# Freerider Watcher

A lightweight, self-hosted web application that watches [Hertz Freerider](https://www.hertzfreerider.se) transport routes and notifies you via **Web Push** (or email fallback) when a matching route becomes available.

> Hertz Freerider lets you drive rental cars between cities for free — but routes fill up fast. This tool keeps watch so you don't have to.

---

## Features

- 🔐 **Authentication** — register, login, password reset via email
- 👁️ **Watches** — define origin/destination, optional time window, weekday filter, one-time or repeating
- 🔔 **Web Push notifications** — VAPID-based push to mobile/desktop browsers
- 📧 **Email fallback** — notifies via SMTP if no push subscription is registered
- 🔄 **Background fetcher** — polls the Hertz Freerider API on a configurable interval with jitter and exponential backoff
- 🗄️ **SQLite** — embedded, disk-based storage; no external database needed
- 🐳 **Docker-first** — single `docker compose up` to run everything
- 🪶 **Minimal footprint** — ~15 MB Alpine image, ~20 MB RAM at idle

## Tech stack

| Layer | Choice | Why |
|-------|--------|-----|
| Language | Go 1.22 | Single binary, low memory, fast startup |
| Database | SQLite (modernc/sqlite — pure Go) | Embedded, no CGO, no external process |
| Push | Web Push / VAPID | Native browser support, no third-party service |
| HTML | Server-rendered Go templates + Pico.css | No build step, minimal JS |
| Container | Alpine 3.19 | ~15 MB base, healthcheck support |

---

## Quick start

### Prerequisites

- Docker + Docker Compose (no Go required on the host)

### 1. Clone and configure

```bash
git clone https://github.com/jagduvi1/freeride-watcher.git
cd freeride-watcher

cp .env.example .env
# Edit .env — at minimum set BASE_URL
```

### 2. Start

```bash
docker compose up --build
# or: make dev
```

The app is now available at **http://localhost:8080**.

### 3. Register an account

Visit `http://localhost:8080/register` and create your first account.

### 4. Enable push notifications

On your dashboard, click **"Enable push notifications"** and allow the browser permission. A test notification is available to verify the setup.

---

## Configuration

All settings are via environment variables (see [`.env.example`](.env.example)).

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | HTTP bind address |
| `BASE_URL` | `http://localhost:8080` | Public URL (used in emails) |
| `DB_PATH` | `/data/freeride.db` | SQLite file path |
| `VAPID_PUBLIC_KEY` | *(auto-generated)* | Web Push public key |
| `VAPID_PRIVATE_KEY` | *(auto-generated)* | Web Push private key |
| `VAPID_SUBJECT` | `mailto:admin@example.com` | VAPID contact |
| `SMTP_HOST` | | SMTP server hostname |
| `SMTP_PORT` | `587` | SMTP port (587=STARTTLS, 465=TLS) |
| `SMTP_USER` | | SMTP username |
| `SMTP_PASS` | | SMTP password |
| `SMTP_FROM` | | From address |
| `FETCH_INTERVAL` | `5m` | API poll interval |
| `FETCH_JITTER` | `30s` | ± random jitter per interval |
| `LOG_LEVEL` | `info` | `info` or `debug` |

### VAPID keys

On first start, VAPID keys are **automatically generated** and persisted in the SQLite database. To survive database resets, copy the keys printed to the log into `.env`:

```
VAPID_PUBLIC_KEY=BN...
VAPID_PRIVATE_KEY=...
```

---

## Running behind a reverse proxy

The app trusts `X-Forwarded-For` for rate limiting. A minimal nginx config:

```nginx
server {
    listen 80;
    server_name freerider.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name freerider.example.com;

    ssl_certificate     /etc/letsencrypt/live/freerider.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/freerider.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

> **Important:** Set `BASE_URL=https://freerider.example.com` in `.env` so session cookies are sent with `Secure=true`.

---

## Development

No local Go installation required — use Docker:

```bash
# Run go mod tidy
make tidy

# Build and run with live reload (restart container on changes)
make dev

# View logs
make logs
```

---

## Data model

```
users          — email + bcrypt password hash
sessions       — server-side sessions (token → user_id + CSRF token)
watches        — user-defined route watches
routes         — routes cached from the Hertz Freerider API
notified       — deduplication log (user_id, route_id) — prevents duplicate alerts
push_subscriptions — Web Push endpoint + keys per device per user
```

---

## License

[MIT](LICENSE)
