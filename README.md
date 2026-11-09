# Sahand-Cortext · v0.2.0

A private, single-user AI workspace hosted on Ubuntu. The browser connects to the hub; provider requests are sent server-side.

## Install

On a clean Ubuntu 22.04/24.04 server:

```bash
curl -fsSL https://raw.githubusercontent.com/sahand363/sahand-cortext/main/install.sh | sudo bash
```

The installer displays numbered progress stages and APT's package progress, creates the first admin login, builds the Go server, and registers a systemd service.

After installation, the terminal prints the panel URL. Open it in your browser, sign in, and use the first-run Gemini setup card to save your API key. Then start chatting.

## SSH control menu

At any time:

```bash
sudo cortext
```

Menu options include:
1. Show panel address
2. Change username and password
3. Configure/renew HTTPS with Caddy
4. Restart service
5. View live logs
6. Uninstall (with a separate confirmation before deleting the database)

## HTTPS

Point a DNS A record to the server and make sure inbound TCP ports 80 and 443 are allowed. Run:

```bash
sudo cortext
```

Choose option 3 and enter the domain and certificate email. Caddy obtains and renews Let's Encrypt certificates automatically.

## Gemini setup

Sign in to the panel and enter the Gemini API key in the setup card or Settings → Gemini provider. The key is stored server-side in SQLite and is never saved in browser local storage. Protect `/var/lib/sahand-cortext` and keep server backups private.

Models in this MVP: `gemini-2.5-flash` and `gemini-2.5-pro`.

## Operations

```bash
sudo systemctl status sahand-cortext
sudo journalctl -u sahand-cortext -f
sudo systemctl restart sahand-cortext
sudo cortext
```

Database: `/var/lib/sahand-cortext/cortext.db`  
Environment: `/etc/sahand-cortext.env`

## Current functionality

- Single-user sign-in (bcrypt password hash, HTTP-only cookie)
- First-run Gemini API key setup inside the web panel
- Gemini chat with model selection
- SQLite conversation persistence and session list
- Responsive desktop/mobile interface
- Settings, provider, memory, orchestration, usage and activity surfaces in the UI
- SSH management menu, systemd, optional Caddy HTTPS

## Planned modules (UI is scaffolded; backend is not yet implemented)

- Provider registry and additional providers
- Multi-model orchestration and coordinator agent
- Topic summaries, topic extraction and relevant memory retrieval
- Token/cost telemetry
- Backup/restore and session export
- API key rotation and encrypted secrets vault

## Security notes

This is an early single-user MVP, not a hardened public multi-tenant service. Use HTTPS for remote access. Add rate limiting, CSRF protections, session persistence/rotation, database encryption or a dedicated secrets store, audit logs, and backup/restore before relying on it for sensitive production workloads. The current chat endpoint sends each prompt as a single-turn request; conversation context assembly is a planned next step.
