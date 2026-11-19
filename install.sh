#!/usr/bin/env bash
set -Eeuo pipefail
APP=sahand-cortext
REPO=https://github.com/sahand363/sahand-cortext.git
# Pick the pre-built binary for this CPU. The release publishes both
# sahand-cortext (amd64) and sahand-cortext-arm64; downloading the wrong
# one would install a binary that cannot run on this machine. If the
# matching asset is missing, the download fails and the installer falls
# back to building locally from source.
ARCH="$(dpkg --print-architecture 2>/dev/null || uname -m)"
case "$ARCH" in
  amd64|x86_64) BIN_ASSET="sahand-cortext" ;;
  arm64|aarch64) BIN_ASSET="sahand-cortext-arm64" ;;
  *) echo "Unsupported CPU architecture: $ARCH" >&2; exit 1 ;;
esac
RELEASE_URL="https://github.com/sahand363/sahand-cortext/releases/latest/download/${BIN_ASSET}"
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
SUMS_URL="https://github.com/sahand363/sahand-cortext/releases/latest/download/SHA256SUMS"
verified_download(){
  local tmp; tmp=$(mktemp -d)
  curl -fL --http1.1 --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 15 --progress-bar "$RELEASE_URL" -o "$tmp/$BIN_ASSET" || { rm -rf "$tmp"; return 1; }
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 15 "$SUMS_URL" -o "$tmp/SHA256SUMS" || { rm -rf "$tmp"; return 1; }
  if ! (cd "$tmp" && grep " ${BIN_ASSET}\$" SHA256SUMS | sha256sum -c --status -); then
    echo -e "${R}Checksum mismatch on the downloaded binary; not installing it.${N}"; rm -rf "$tmp"; return 1
  fi
  install -m 755 "$tmp/$BIN_ASSET" "${BIN}.tmp" && mv -f "${BIN}.tmp" "$BIN"
  rm -rf "$tmp"
}
if verified_download; then
  echo -e "${G}✓ Pre-built binary downloaded, checksum verified and installed.${N}"
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
  /usr/local/go/bin/go mod download
  CGO_ENABLED=0 /usr/local/go/bin/go build -trimpath -ldflags="-s -w" -o "${BIN}.tmp" ./cmd/server
  chmod +x "${BIN}.tmp"
  mv -f "${BIN}.tmp" "$BIN"
fi

chmod +x "$BIN"
if [[ ! -x "$BIN" ]]; then
  echo -e "${R}Installed binary is not executable: $BIN${N}"
  ls -l "$BIN"
  exit 1
fi

step 80 "Verifying service account and credentials"
id cortext >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin cortext
install -d -o cortext -g cortext -m 750 "$DATA"

if [[ -f "$ENV" ]] && grep -Eq '^HUB_PASSWORD_HASH=\$2[aby]\$' "$ENV"; then
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
  printf 'HUB_ADDR=0.0.0.0:8787\nHUB_DATA_DIR=%s\nHUB_WEB_DIR=%s/web/dist\nHUB_USERNAME=%s\nHUB_PASSWORD_HASH=%s\nHUB_COOKIE_SECURE=false\n' \
    "$DATA" "$DIR" "$username" "$hash" > "$ENV"
  chmod 600 "$ENV"
  unset p1 p2 hash
fi

# Ensure the API-key encryption key exists (kept in the env file, outside the
# database, so a leaked database alone does not expose the API keys).
if ! grep -q '^HUB_KEY_ENCRYPTION_KEY=' "$ENV" 2>/dev/null; then
  enc_key=$(openssl rand -base64 32)
  printf 'HUB_KEY_ENCRYPTION_KEY=%s\n' "$enc_key" >> "$ENV"
  chmod 600 "$ENV"
  unset enc_key
  echo -e "${G}✓ API-key encryption key generated.${N}"
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
WatchdogSec=30
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

sleep 5
# A process can be "active" while hung, so verify the panel itself answers,
# not just that systemd thinks the service is running.
addr=$(grep '^HUB_ADDR=' "$ENV" 2>/dev/null | cut -d= -f2); port="${addr##*:}"; port="${port:-8787}"
if ! systemctl is-active --quiet "$APP" || ! curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:${port}/"; then
  echo -e "${R}Service failed to start or is not answering after install/update.${N}"
  systemctl status "$APP" --no-pager | head -n 20 || true
  journalctl -u "$APP" -n 20 --no-pager || true
  exit 1
fi

step 100 "Done!"
if [[ "$MODE" == "--update" || "$MODE" == "update" ]]; then
  ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  echo -e "\n${G}✓ Sahand-CorText updated and restarted! Panel: http://${ip}:8787${N}"
  exit 0
fi

if [[ -r /dev/tty ]]; then
  INSTALLED_NOW=1 exec "$MENU" </dev/tty
fi
