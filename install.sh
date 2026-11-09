#!/usr/bin/env bash
set -Eeuo pipefail
APP=sahand-cortext
REPO=https://github.com/sahand363/sahand-cortext.git
DIR=/opt/sahand-cortext
DATA=/var/lib/sahand-cortext
ENV=/etc/sahand-cortext.env
SERVICE=/etc/systemd/system/sahand-cortext.service
BIN=/usr/local/bin/sahand-cortext
MENU=/usr/local/bin/cortext
G='\033[0;32m'; C='\033[0;36m'; Y='\033[1;33m'; R='\033[0;31m'; N='\033[0m'
step(){ printf "\n${C}[%3s%%]${N} %s\n" "$1" "$2"; }
fail(){ echo -e "${R}Installation failed. Check the error above.${N}"; exit 1; }
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
echo -e "${N}${G}Sahand-CorText · Ubuntu Installer${N}"

step 5 "Checking system and privileges"
. /etc/os-release
[[ "${ID:-}" == ubuntu ]] || echo -e "${Y}This installer is designed for Ubuntu.${N}"

step 15 "Installing system dependencies (APT progress is shown below)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -o Dpkg::Progress-Fancy=1
apt-get install -y -o Dpkg::Progress-Fancy=1 git curl ca-certificates openssl gnupg

step 30 "Preparing Go toolchain"
export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"
if ! command -v go >/dev/null 2>&1 || ! go version | grep -Eq 'go1\.(23|24|25)'; then
  arch=$(dpkg --print-architecture)
  case "$arch" in
    amd64) ga=amd64;;
    arm64) ga=arm64;;
    *) echo "Unsupported CPU architecture: $arch"; exit 1;;
  esac
  curl -fL --progress-bar "https://go.dev/dl/go1.23.12.linux-${ga}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  rm -f /tmp/go.tgz
fi

step 40 "Downloading Sahand-CorText source"
if [[ -d "$DIR/.git" ]]; then
  git -C "$DIR" fetch --all --prune
  git -C "$DIR" reset --hard origin/main
else
  rm -rf "$DIR"
  git clone --progress "$REPO" "$DIR"
fi

step 55 "Resolving Go dependencies and building application"
cd "$DIR"
go mod tidy
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/server

step 70 "Creating restricted service account and data directory"
id cortext >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin cortext
install -d -o cortext -g cortext -m 750 "$DATA"

step 80 "Creating your first admin account"
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

step 90 "Registering system service and SSH control menu"
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
systemctl enable --now "$APP"

step 100 "Installation complete"
if [[ -r /dev/tty ]]; then
  INSTALLED_NOW=1 exec "$MENU" </dev/tty
fi
