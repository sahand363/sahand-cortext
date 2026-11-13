# Sahand-CorText · v0.2.2

**A private, secure, single-user AI workspace hosted on your own Ubuntu server.**

Sahand-CorText provides a private AI gateway where browser communication goes directly to your self-hosted server, while upstream AI provider requests are routed server-side to protect your client IP address.

---

## 🚀 Quick Installation

### One-Line Installer

On a clean Ubuntu 22.04 or 24.04 server, run the following command with root privileges:

```bash
curl -fsSL https://raw.githubusercontent.com/sahand363/sahand-cortext/main/install.sh | sudo bash
```

The installer automatically:

- Checks and installs required prerequisites.
- Downloads the latest optimized pre-built release.
- Creates a restricted system service account (`cortext`).
- Registers a sandboxed `systemd` service.
- Configures the application environment.
- Sets up your initial administrator credentials.

Once installation is complete, access your workspace using the displayed panel address.

---

## 🎛️ Server Administration

Sahand-CorText includes an interactive command-line management utility directly accessible from your server terminal.

Run:

```bash
sudo cortext
```

### Control Center Menu

| Option | Description |
|---|---|
| **1. Update Sahand-CorText** | Installs the latest application binary and web interface assets without repeating the full installation process. |
| **2. Show Panel Address** | Displays your local and remote panel URLs. |
| **3. Change Credentials** | Securely updates your administrator username and password. |
| **4. Configure / Renew SSL** | Installs Caddy, provisions Let's Encrypt certificates, and configures HTTPS and secure cookie flags. |
| **5. Restart Service** | Restarts the background application service. |
| **6. View Live Logs** | Streams real-time application logs using `journalctl`. |
| **7. Uninstall** | Removes application binaries, system services, and optionally database files. |

**Quick Update Command**

You can also update the application directly without opening the management menu:

```bash
sudo cortext update
```

---

## ✳️ Gemini AI Setup & Model Verification

Follow these steps to connect your Google Gemini account:

1. Open your browser and navigate to your server panel:

   ```text
   http://YOUR-SERVER-IP:8787
   ```

2. Log in using your administrator credentials.

3. Navigate to **Settings → AI Providers**, or click the **Set up Gemini** banner.

4. Enter your **Google AI Studio API Key** and click **Save Key & Load Models**.

5. Sahand-CorText queries Google's live API model catalog and displays the models available to your API key.

6. Click any model card to perform a live generation test.

   Successfully tested models are highlighted in green, displaying their measured response latency:

   ```text
   ✓ WORKING · 180 ms
   ```

   The successfully tested model is automatically selected as your active chat model.

---

## 🛠️ Technology Stack

| Component | Technology |
|---|---|
| **Server OS** | Ubuntu Linux (AMD64 & ARM64) |
| **Backend** | Go (`net/http`, standard library concurrency) |
| **Database** | Embedded SQLite (`modernc.org/sqlite`, WAL mode) |
| **Authentication** | Bcrypt password hashing + persistent SQLite sessions |
| **Frontend** | Vanilla HTML5, CSS3, and JavaScript |
| **Deployment** | Pre-built static binary via GitHub Actions |
| **Service Management** | Sandboxed `systemd` service |
| **Reverse Proxy / SSL** | Caddy + Let's Encrypt (optional) |

---

## 📋 Core Capabilities

### 🔐 Private AI Gateway
All upstream AI provider requests originate from your Ubuntu server rather than your local browser, keeping your client IP address separate from provider-facing requests.

### 🔍 Live Model Discovery & Testing
Dynamically discovers available Gemini `generateContent` models based on your API key and verifies model availability through live generation tests.

### 💬 Persistent Conversations
- SQLite-backed conversation storage.
- Persistent chat history across server restarts.
- Automatic conversation title generation based on the first prompt.

### 🛡️ Secure Session Management
- HTTP-only session cookies.
- Bcrypt password hashing.
- Persistent SQLite-backed sessions.
- Multi-day session persistence across server restarts.

### 👤 Single-User Architecture
Designed as a private personal AI workspace rather than a public, multi-tenant SaaS platform.

---

## 🗺️ Roadmap

The following modules are planned for future releases:

- [ ] **Multi-Provider Registry:** Native integrations for OpenAI (GPT), Anthropic (Claude), DeepSeek, and custom OpenAI-compatible endpoints.
- [ ] **AI Orchestration Layer:** Multi-agent coordination with intelligent task routing across specialized models.
- [ ] **Topic-Based Memory:** Cross-session vector or keyword-based memory retrieval for long-term project context.
- [ ] **Token & Cost Telemetry:** Usage analytics, token consumption tracking, and request latency history.

---

## 📂 Installation Directories

| Path | Description |
|---|---|
| `/opt/sahand-cortext/` | Application root directory and web assets (`web/dist/`). |
| `/var/lib/sahand-cortext/` | Persistent application data and SQLite database (`cortext.db`). |
| `/etc/sahand-cortext.env` | Environment configuration and security keys. |
| `/etc/systemd/system/sahand-cortext.service` | Background system service configuration. |
| `/usr/local/bin/cortext` | SSH-based management CLI utility. |

---

## 🔒 Security Recommendations

- **Enable HTTPS:** Always configure SSL using `sudo cortext` → Option 4 before exposing your panel to the public internet.
- **Protect Application Data:** Restrict access to `/var/lib/sahand-cortext/` and maintain regular database backups.
- **Use Strong Credentials:** Choose a strong administrator password and rotate it periodically through the SSH Control Center.
- **Keep Updated:** Regularly install the latest releases using `sudo cortext update`.
- **Protect API Keys:** Never share your provider API keys or expose them in frontend code.

---

## 📄 License

See the [LICENSE](LICENSE) file for licensing information.

---

<p align="center">
  <b>Sahand-CorText</b><br>
  Private Intelligence. Your Server. Your Control.
</p>
