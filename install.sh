#!/usr/bin/env bash
set -Eeuo pipefail

APP="sahand-cortext"
REPO="https://github.com/sahand363/sahand-cortext.git"
INSTALL_DIR="/opt/${APP}"
DATA_DIR="/var/lib/${APP}"
ENV_FILE="/etc/${APP}.env"
SERVICE="/etc/systemd/system/${APP}.service"
BIN="/usr/local/bin/${APP}"
GREEN='\033[0;32m'; CYAN='\033[0;36m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; RESET='\033[0m'

banner(){ clear 2>/dev/null || true; echo -e "${CYAN}"; cat <<'ART'
  ____        _                 _      ____          _            _
 / ___|  __ _| |__   __ _ _ __ | |  / ___|___  _ __| |_ _____  _| |_
 \___ \ / _` | '_ \ / _` | '_ \| | | |   / _ \| '__| __/ _ \ \/ / __|
  ___) | (_| | | | | (_| | | | | | | |__| (_) | |  | ||  __/>  <| |_
 |____/ \__, |_| |_|\__,_|_| |_|_|  \____\___/|_|   \__\___/_/\_\\__|
        |___/
ART
echo -e "${RESET}${YELLOW}Private AI workspace · Ubuntu installer${RESET}\n"; }
need_root(){ [[ -r /dev/tty ]] || { echo "Run this installer from an interactive terminal."; exit 1; }; [[ $EUID -eq 0 ]] || { echo -e "${RED}Run with sudo: sudo bash install.sh${RESET}"; exit 1; }; }
ask(){ local prompt="$1" default="$2" value; read -rp "$prompt [$default]: " value </dev/tty; echo "${value:-$default}"; }
install_deps(){ echo -e "${CYAN}› Installing system dependencies…${RESET}"; apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git curl ca-certificates openssl gnupg
  if ! command -v go >/dev/null 2>&1 || ! go version | grep -q 'go1.23\|go1.24\|go1.25'; then
    ARCH="$(dpkg --print-architecture)"
    case "$ARCH" in amd64) GOARCH=amd64 ;; arm64) GOARCH=arm64 ;; *) echo "Unsupported architecture: $ARCH"; exit 1 ;; esac
    curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go1.23.12.linux-${GOARCH}.tar.gz"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go.tgz
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    rm -f /tmp/go.tgz
  fi
}
get_source(){
  if [[ -d "$INSTALL_DIR/.git" ]]; then git -C "$INSTALL_DIR" pull --ff-only
  else git clone "$REPO" "$INSTALL_DIR"; fi
}
setup_user(){ id cortext >/dev/null 2>&1 || useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin cortext; install -d -o cortext -g cortext -m 750 "$DATA_DIR"; }
setup_env(){
  echo -e "\n${CYAN}› Configure your private workspace${RESET}"
  USERNAME="$(ask 'Panel username' 'admin')"
  while true; do read -rsp "Panel password (minimum 12 characters): " PASS1 </dev/tty; echo; [[ ${#PASS1} -ge 12 ]] && break; echo -e "${RED}Password must be at least 12 characters.${RESET}"; done
  read -rsp "Confirm password: " PASS2 </dev/tty; echo; [[ "$PASS1" == "$PASS2" ]] || { echo "Passwords do not match."; exit 1; }
  read -rsp "Gemini API key (leave blank to add later): " GEMINI_KEY </dev/tty; echo
  HASH="$("$BIN" hash-password "$PASS1")"
  umask 077
  cat > "$ENV_FILE" <<EOF
HUB_ADDR=127.0.0.1:8787
HUB_DATA_DIR=$DATA_DIR
HUB_WEB_DIR=$INSTALL_DIR/web/dist
HUB_USERNAME=$USERNAME
HUB_PASSWORD_HASH=$HASH
HUB_COOKIE_SECURE=false
GEMINI_API_KEY=$GEMINI_KEY
EOF
  chmod 600 "$ENV_FILE"; unset PASS1 PASS2 GEMINI_KEY HASH
}
setup_service(){
  cat > "$SERVICE" <<EOF
[Unit]
Description=Sahand-Cortext private AI hub
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=cortext
Group=cortext
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=on-failure
RestartSec=4
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR
CapabilityBoundingSet=
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload; systemctl enable --now "$APP"
}
setup_ssl(){
  echo -e "\n${CYAN}› Optional HTTPS setup${RESET}"
  DOMAIN="$(ask 'Domain name (leave blank to skip SSL)' '')"
  [[ -z "$DOMAIN" ]] && return
  EMAIL="$(ask 'Email for Let’s Encrypt notices' '')"
  if ! command -v caddy >/dev/null 2>&1; then
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
    apt-get update -qq
    apt-get install -y -qq caddy
  fi
  if [[ -n "$EMAIL" ]]; then
    cat > /etc/caddy/Caddyfile <<EOF
{
    email $EMAIL
}
$DOMAIN {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8787
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options "nosniff"
        X-Frame-Options "DENY"
    }
}
EOF
  else
    cat > /etc/caddy/Caddyfile <<EOF
$DOMAIN {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8787
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options "nosniff"
        X-Frame-Options "DENY"
    }
}
EOF
  fi
  systemctl enable --now caddy; systemctl reload caddy
  sed -i 's/HUB_COOKIE_SECURE=false/HUB_COOKIE_SECURE=true/' "$ENV_FILE"
  systemctl restart "$APP"
  PANEL_URL="https://$DOMAIN"
}
main(){
 banner; need_root; install_deps
 echo -e "${CYAN}› Fetching Sahand-Cortext source…${RESET}"; get_source
 echo -e "${CYAN}› Building application…${RESET}"
 cd "$INSTALL_DIR"; go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/server
 setup_user; setup_env; setup_service
 PANEL_URL="http://127.0.0.1:8787 (SSH tunnel required without a domain)"
 setup_ssl
 echo -e "\n${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
 echo -e "${GREEN}  Sahand-Cortext is installed and running.${RESET}"
 echo -e "  Panel: ${CYAN}${PANEL_URL}${RESET}"
 echo -e "  Service: systemctl status $APP"
 echo -e "  Logs: journalctl -u $APP -f"
 echo -e "  Config: $ENV_FILE"
 if [[ "$PANEL_URL" == *"SSH tunnel required"* ]]; then
   echo -e "  Remote access: ssh -L 8787:127.0.0.1:8787 YOUR_USER@YOUR_SERVER"
   echo -e "  Then open: http://127.0.0.1:8787"
   echo -e "  For normal mobile/desktop access, rerun SSL setup with a domain."
 fi
 echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}\n"
}
main "$@"
