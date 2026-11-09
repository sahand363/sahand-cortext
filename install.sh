#!/usr/bin/env bash
set -Eeuo pipefail
APP=sahand-cortext
REPO=https://github.com/sahand363/sahand-cortext.git
RELEASE_URL=https://github.com/sahand363/sahand-cortext/releases/latest/download/sahand-cortext
DIR=/opt/sahand-cortext
DATA=/var/lib/sahand-cortext
ENV=/etc/sahand-cortext.env
SERVICE=/etc/systemd/system/sahand-cortext.service
BIN=/usr/local/bin/sahand-cortext
MENU=/usr/local/bin/cortext
MODE="${1:-install}"

G='\033[0;32m'; C='\033[0;36m'; Y='\033[1;33m'; R='\033[0;31m'; N='\033[0m'
step(){ printf "\n${C}[%3s%%]${N} %s\n" "$1" "$2"; }
fail(){ echo -e "${R}Operation failed. Check the error above.${N}"; exit 1; }
trap fail ERR

[[ $EUID -eq 0 ]] || { echo "Run with sudo: sudo bash install.sh"; exit 1; }
clear 2>/dev/null || true
echo -e "${C}"
cat <<'EOF'
  _____       _                     _        _____          _______        _   
 / ____|     | |                   | |      / ____|        |__   __|      | |  
| (___   __ _| |__   __ _ _ __   __| |_____| |     ___  _ __  | | _____  _| |_ 
 \___ \ / _` | '_ \ / _` | '_ \ / _` |_____| |    / _ \| '__| | |/ _ \ \/ / __|
 ____) | (_| | | | | (_| | | | | (_| |     | |___| (_) | |    | |  __/>  <| |_ 
|_____/ \__,_|_| |_|\__,_|_| |_|\__,_|      \_____\___/|_|    |_|\___/_/\_\\__|
EOF
if [[ "$MODE" == "--update" || "$MODE" == "update" ]]; then
  echo -e "${N}${G}Sahand-CorText · Fast Updater${N}"
else
  echo -e "${N}${G}Sahand-CorText · Fast Installer & Updater${N}"
fi

step 10 "Checking required system tools"
if command -v git >/dev/null 2>&1 && command -v curl >/dev/null 2>&1 && command -v openssl >/dev/null 2>&1; then
  echo -e "${G}✓ System tools (git, curl, openssl) are already installed — skipping APT download.${N}"
else
  sed -i 's|http://de\.archive\.ubuntu\.com/ubuntu|http://archive.ubuntu.com/ubuntu|g' /etc/apt/sources.list 2>/dev/null || true
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -o Dpkg::Progress-Fancy=1
  apt-get install -y -o Dpkg::Progress-Fancy=1 git curl ca-certificates openssl gnupg
fi

step 35 "Updating Sahand-CorText web files"
if [[ -d "$DIR/.git" ]]; then
  git -C "$DIR" fetch --depth 1 origin main
  git -C "$DIR" reset --hard origin/main
else
  rm -rf "$DIR"
  git clone --depth 1 "$REPO" "$DIR"
fi

step 60 "Installing application binary"
mkdir -p /usr/local/bin
rm -f "${BIN}.tmp"
if curl -fL --http1.1 --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 15 --progress-bar "$RELEASE_URL" -o "${BIN}.tmp"; then
  chmod +x "${BIN}.tmp"
  mv -f "${BIN}.tmp" "$BIN"
  echo -e "${G}✓ Pre-built binary downloaded and installed.${N}"
else
  rm -f "${BIN}.tmp"
  echo -e "${Y}Release download interrupted. Building binary with Go 1.23...${N}"
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -Eq 'go1\.(23|24|25)'; then
    arch=$(dpkg --print-architecture)
    case "$arch" in amd64) ga=amd64;; arm64) ga=arm64;; *) echo "Unsupported CPU: $arch"; exit 1;; esac
    curl -fL --http1.1 --retry 5 --retry-delay 2 --retry-all-errors --progress-bar "https://go.dev/dl/go1.23.12.linux-${ga}.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go.tgz
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    rm -f /tmp/go.tgz
  fi
  export PATH="/usr/local/go/bin:$PATH"
  cd "$DIR"
  /usr/local/go/bin/go mod tidy
  CGO_ENABLED=0 /usr/local/go/bin/go build -trimpath -ldflags="-s -w" -o "${BIN}.tmp" ./cmd/server
  mv -f "${BIN}.tmp" "$BIN"
fi

step 80 "Verifying service account and credentials"
id cortext >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin cortext
install -d -o cortext -g cortext -m 750 "$DATA"

if [[ -f "$ENV" ]] && grep -q "^HUB_PASSWORD_HASH=" "$ENV"; then
  echo -e "${G}✓ Existing admin credentials and database preserved.${N}"
else
  read -rp "Panel username [admin]: " username </dev/tty
  username=${username:-admin}
  while true; do
    read -rsp "Panel password (at least 12 characters): " p1 </dev/tty
    echo
    [[ ${#p1} -ge 12 ]] && break
    echo "Use at least 12 characters."
  done
  read -rsp "Confirm password: " p2 </dev/tty
  echo
  [[ "$p1" == "$p2" ]] || { echo "Passwords do not match."; exit 1; }
  hash=$("$BIN" hash-password "$p1")
  umask 077
  cat > "$ENV" <<EOF
HUB_ADDR=0.0.0.0:8787
HUB_DATA_DIR=$DATA
HUB_WEB_DIR=$DIR/web/dist
HUB_USERNAME=$username
HUB_PASSWORD_HASH=$hash
HUB_COOKIE_SECURE=false
EOF
  chmod 600 "$ENV"
  unset p1 p2 hash
fi

step 95 "Updating system service and SSH menu"
cat > "$SERVICE" <<EOF
[Unit]
Description=Sahand-CorText private AI hub
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=cortext
Group=cortext
WorkingDirectory=$DIR
EnvironmentFile=$ENV
ExecStart=$BIN
Restart=on-failure
RestartSec=4
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA
CapabilityBoundingSet=
LockPersonality=true
[Install]
WantedBy=multi-user.target
EOF
install -m 755 "$DIR/scripts/cortext-menu.sh" "$MENU"
systemctl daemon-reload
systemctl enable "$APP" >/dev/null 2>&1 || true
systemctl restart "$APP"

step 100 "Done!"
if [[ "$MODE" == "--update" || "$MODE" == "update" ]]; then
  ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  echo -e "\n${G}✓ Sahand-CorText updated and restarted! Panel: http://${ip}:8787${N}"
  exit 0
fi

if [[ -r /dev/tty ]]; then
  INSTALLED_NOW=1 exec "$MENU" </dev/tty
fi
