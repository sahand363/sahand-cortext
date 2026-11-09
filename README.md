# Sahand-Cortext

A lightweight, single-user private AI hub. The Ubuntu server hosts the web interface and sends provider requests server-side.

## Current MVP

- Single-user login with bcrypt password hash and HTTP-only session cookie
- Gemini chat (Gemini 3.8 Flash / Gemini 4 Argon)
- Persistent conversations and messages in SQLite
- Model selection
- Server/provider status
- Minimal responsive web UI
- systemd service and optional Caddy HTTPS

Modules for additional providers, orchestration, topic memory, token analytics, and backups are represented in the UI/architecture but are not implemented yet.

## Install on Ubuntu

Use a clean Ubuntu 22.04/24.04 server. Point a domain's DNS A record to the server first if you want HTTPS.

```bash
curl -fsSL https://raw.githubusercontent.com/sahand363/sahand-cortext/main/install.sh | sudo bash
```

The installer installs dependencies, clones this repository, builds the Go server, asks for a panel username/password and optional Gemini API key, creates a restricted system user, and registers a systemd service.

If you skip the domain, the app remains bound to localhost for safety. Use an SSH tunnel (`ssh -L 8787:127.0.0.1:8787 YOUR_USER@YOUR_SERVER`) and open `http://127.0.0.1:8787`. For normal access from Windows/mobile browsers, configure a domain with DNS pointing to the server and enable Caddy HTTPS during installation. Do not expose the app over plain HTTP on an untrusted network.

## Gemini API key

Create a key in Google AI Studio and provide it during installation. To update it later:

```bash
sudo nano /etc/sahand-cortext.env
sudo systemctl restart sahand-cortext
```

Keep this file private; it contains the provider key and password hash. The key is never sent to the browser.

## Useful commands

```bash
sudo systemctl status sahand-cortext
sudo journalctl -u sahand-cortext -f
sudo systemctl restart sahand-cortext
```

Database and application data: `/var/lib/sahand-cortext/cortext.db`

## Local development

Requires Go 1.23+.

```bash
go mod tidy
go run ./cmd/server
```

Set environment variables before running:

```bash
export HUB_USERNAME=admin
export HUB_PASSWORD_HASH='<bcrypt hash>'
export HUB_DATA_DIR=./data
export HUB_WEB_DIR=./web/dist
export HUB_ADDR=127.0.0.1:8787
export GEMINI_API_KEY='your-key'
go run ./cmd/server
```

Generate a bcrypt password hash:

```bash
go run ./cmd/server hash-password 'your-long-password'
```

## Security notes

- The service binds to `127.0.0.1:8787`; Caddy is the recommended public HTTPS reverse proxy.
- The installer creates a dedicated unprivileged `cortext` account.
- SQLite data is stored outside the Git checkout.
- API keys are stored in a root-readable environment file, not in browser storage.
- Sessions are in-memory and expire after 24 hours; restarting the service signs users out.
- This is an early MVP. Add rate limiting, CSRF defenses beyond SameSite cookies, session persistence/rotation, encrypted secrets, backup/restore, and stronger operational monitoring before treating it as a hardened production service.

## Project layout

```text
sahand-cortext/
├── cmd/server/main.go
├── web/dist/
│   ├── index.html
│   ├── style.css
│   └── app.js
├── install.sh
├── go.mod
├── .gitignore
└── README.md
```

## Roadmap

1. Provider interface and additional AI providers
2. Secure provider-key management and health checks
3. Token usage and cost telemetry
4. Topic memory with configurable retrieval
5. Multi-model orchestration / agent coordinator
6. Backup and restore
7. GitHub Actions release artifacts
