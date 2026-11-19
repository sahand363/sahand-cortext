#!/usr/bin/env bash
set -Eeuo pipefail
APP=sahand-cortext
ENV_FILE=/etc/sahand-cortext.env
SERVICE=/etc/systemd/system/sahand-cortext.service
BIN=/usr/local/bin/sahand-cortext
DIR=/opt/sahand-cortext
DATA=/var/lib/sahand-cortext
CADDY=/etc/caddy/Caddyfile
# Pick the pre-built binary for this CPU. The release publishes both
# sahand-cortext (amd64) and sahand-cortext-arm64; downloading the wrong
# one would install a binary that cannot run on this machine. If the
# matching asset is missing, the download fails and the updater falls
# back to building locally from source.
ARCH="$(dpkg --print-architecture 2>/dev/null || uname -m)"
case "$ARCH" in
  amd64|x86_64) BIN_ASSET="sahand-cortext" ;;
  arm64|aarch64) BIN_ASSET="sahand-cortext-arm64" ;;
  *) echo "Unsupported CPU architecture: $ARCH" >&2; exit 1 ;;
esac
RELEASE_URL="https://github.com/sahand363/sahand-cortext/releases/latest/download/${BIN_ASSET}"

G='\033[0;32m'; C='\033[0;36m'; Y='\033[1;33m'; R='\033[0;31m'; N='\033[0m'
[[ $EUID -eq 0 ]] || { echo "Run: sudo cortext"; exit 1; }

getv(){ grep -m1 "^$1=" "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true; }

panel_url(){
  if command -v caddy >/dev/null 2>&1 && [[ -f "$CADDY" ]]; then
    host=$(awk 'NF && $1 !~ /^#/ && $1 !~ /^\{/ {print $1; exit}' "$CADDY")
    [[ "$host" == *.* ]] && { echo "https://$host"; return; }
  fi
  ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  [[ -z "$ip" ]] && ip=$(curl -4fsSL --max-time 2 https://api.ipify.org 2>/dev/null || echo "SERVER-IP")
  echo "http://${ip}:8787"
}

pause(){ echo; read -rp "Press Enter to return to menu…" _ </dev/tty; }

header(){
  clear 2>/dev/null || true
  echo -e "${G}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
  if [[ "${INSTALLED_NOW:-0}" == "1" ]]; then
    echo -e "  ${G}✓ INSTALLATION COMPLETE — SAHAND-CORTEXT IS RUNNING${N}"
  fi
  echo -e "  ${Y}PANEL LINK :${N} ${C}$(panel_url)${N}"
  echo -e "  ${Y}VERSION    :${N} v$(version_of "$DIR")$( [[ -d "${DIR}.prev" ]] && echo "   (rollback to v$(version_of "${DIR}.prev") available)")"
  echo -e "  ${Y}STATUS     :${N} $(systemctl is-active "$APP" 2>/dev/null || echo unknown)   |   ${Y}QUICK UPDATE :${N} sudo cortext update"
  echo -e "${G}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${N}"
  echo -e "${C}"
  cat <<'EOF'
  _____       _                     _        _____          _______        _   
 / ____|     | |                   | |      / ____|        |__   __|      | |  
| (___   __ _| |__   __ _ _ __   __| |_____| |     ___  _ __  | | _____  _| |_ 
 \___ \ / _` | '_ \ / _` | '_ \ / _` |_____| |    / _ \| '__| | |/ _ \ \/ / __|
 ____) | (_| | | | | (_| | | | | (_| |     | |___| (_) | |    | |  __/>  <| |_ 
|_____/ \__,_|_| |_|\__,_|_| |_|\__,_|      \_____\___/|_|    |_|\___/_/\_\\__|
EOF
  echo -e "${N}"
  echo -e "  ${G}SAHAND-CORTEXT CONTROL CENTER${N}"
  echo
}

SUMS_URL="https://github.com/sahand363/sahand-cortext/releases/latest/download/SHA256SUMS"

# Version of an installed copy, read from its source tree (never by running
# the binary: old binaries ignore "version" and would start a second server).
version_of(){ grep -m1 'const AppVersion' "$1/cmd/server/main.go" 2>/dev/null | cut -d'"' -f2 || true; }

# Wait up to ~20s for the service to be active and answer HTTP.
health_ok(){
  local addr port i
  addr=$(getv HUB_ADDR); port="${addr##*:}"; port="${port:-8787}"
  for i in 1 2 3 4 5 6 7 8 9 10; do
    sleep 2
    systemctl is-active --quiet "$APP" && curl -fsS -o /dev/null --max-time 3 "http://127.0.0.1:${port}/" && return 0
  done
  return 1
}

# Keep the currently installed version so it can be restored.
snapshot_current(){
  [[ -x "$BIN" && -d "$DIR" ]] || return 0
  cp -a "$BIN" "${BIN}.prev"
  rm -rf "${DIR}.prev"
  cp -a "$DIR" "${DIR}.prev"
  echo -e "${G}✓ Saved current version (v$(version_of "${DIR}.prev")) for rollback.${N}"
}

# Download the release binary and install it only if its SHA-256 matches
# the published SHA256SUMS. Returns 1 on any download or checksum problem.
install_verified_binary(){
  local tmp; tmp=$(mktemp -d)
  if ! curl -fL --http1.1 --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 15 --progress-bar "$RELEASE_URL" -o "$tmp/$BIN_ASSET" \
     || ! curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --connect-timeout 15 "$SUMS_URL" -o "$tmp/SHA256SUMS"; then
    rm -rf "$tmp"; echo -e "${Y}Release download failed.${N}"; return 1
  fi
  if ! (cd "$tmp" && grep " ${BIN_ASSET}\$" SHA256SUMS | sha256sum -c --status -); then
    rm -rf "$tmp"; echo -e "${R}Checksum mismatch: the downloaded binary is corrupt or was tampered with. Not installing it.${N}"; return 1
  fi
  install -m 755 "$tmp/$BIN_ASSET" "${BIN}.tmp" && mv -f "${BIN}.tmp" "$BIN"
  rm -rf "$tmp"
  echo -e "${G}✓ Binary downloaded and checksum verified.${N}"
}

build_from_source(){
  echo -e "${Y}Building locally with Go 1.23 from the pinned sources...${N}"
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -Eq 'go1\.(23|24|25)'; then
    arch=$(dpkg --print-architecture)
    case "$arch" in amd64) ga=amd64;; arm64) ga=arm64;; *) exit 1;; esac
    curl -fL --http1.1 --retry 5 --retry-delay 2 --retry-all-errors --progress-bar "https://go.dev/dl/go1.23.12.linux-${ga}.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  fi
  (cd "$DIR" && /usr/local/go/bin/go mod download && CGO_ENABLED=0 /usr/local/go/bin/go build -trimpath -ldflags="-s -w" -o "${BIN}.tmp" ./cmd/server)
  chmod +x "${BIN}.tmp"
  mv -f "${BIN}.tmp" "$BIN"
}

# Swap the installed version with the saved one. Running it twice returns
# to where you started.
swap_with_prev(){
  mv -f "$BIN" "${BIN}.swap" && mv -f "${BIN}.prev" "$BIN" && mv -f "${BIN}.swap" "${BIN}.prev"
  rm -rf "${DIR}.swap"
  mv "$DIR" "${DIR}.swap" && mv "${DIR}.prev" "$DIR" && mv "${DIR}.swap" "${DIR}.prev"
  install -m 755 "$DIR/scripts/cortext-menu.sh" /usr/local/bin/cortext
  systemctl restart "$APP"
}

rollback_app(){
  if [[ ! -x "${BIN}.prev" || ! -d "${DIR}.prev" ]]; then
    echo -e "${Y}No previous version is saved yet. One is kept automatically on every update.${N}"
    return 1
  fi
  local cur prev; cur=$(version_of "$DIR"); prev=$(version_of "${DIR}.prev")
  if [[ "${1:-}" != "--yes" ]]; then
    echo -e "Installed: ${C}v${cur:-?}${N}   Saved: ${C}v${prev:-?}${N}"
    echo -e "${Y}Your data, settings and chats are kept. Features added after v${prev:-?} stop working until you update again.${N}"
    read -rp "Roll back to v${prev:-?}? [y/N]: " ok </dev/tty
    [[ "$ok" =~ ^[Yy]$ ]] || { echo "Cancelled."; return 0; }
  fi
  echo -e "${C}Restoring v${prev:-?}...${N}"
  swap_with_prev
  if health_ok; then
    echo -e "${G}✓ Rolled back to v${prev:-?}. v${cur:-?} is kept, so 'sudo cortext update' moves you forward again.${N}"
  else
    echo -e "${R}v${prev:-?} isn't answering either. Check the logs: journalctl -u $APP -n 50${N}"
    return 1
  fi
}

update_app(){
  echo -e "\n${C}[1/4] Saving the current version for rollback...${N}"
  snapshot_current

  echo -e "${C}[2/4] Pulling latest web interface and scripts from GitHub...${N}"
  if [[ -d "$DIR/.git" ]]; then
    git -C "$DIR" fetch --depth 1 origin main
    git -C "$DIR" reset --hard origin/main
  else
    rm -rf "$DIR"
    git clone --depth 1 https://github.com/sahand363/sahand-cortext.git "$DIR"
  fi

  echo -e "${C}[3/4] Downloading latest pre-built server binary...${N}"
  install_verified_binary || build_from_source

  chmod +x "$BIN"
  # Ensure the API-key encryption key exists (kept in the env file, outside the
  # database, so a leaked database alone does not expose the API keys).
  if ! grep -q '^HUB_KEY_ENCRYPTION_KEY=' "$ENV_FILE" 2>/dev/null; then
    enc_key=$(openssl rand -base64 32)
    printf 'HUB_KEY_ENCRYPTION_KEY=%s\n' "$enc_key" >> "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    unset enc_key
    echo -e "${G}✓ API-key encryption key generated.${N}"
  fi
  echo -e "${C}[4/4] Restarting service...${N}"
  install -m 755 "$DIR/scripts/cortext-menu.sh" /usr/local/bin/cortext
  # Ensure the watchdog is enabled so a hung (not just crashed) service is
  # restarted by systemd. Idempotent; only adds the line if it is missing.
  if ! grep -q '^WatchdogSec=' "$SERVICE" 2>/dev/null; then
    sed -i '/^RestartSec=/a WatchdogSec=30' "$SERVICE"
  fi
  systemctl daemon-reload
  systemctl restart "$APP"
  # Verify the panel itself answers; a process can be "active" while hung.
  if health_ok; then
    echo -e "\n${G}✓ Sahand-CorText v$(version_of "$DIR") is running! Panel: $(panel_url)${N}"
    return 0
  fi
  echo -e "${R}The new version did not come up.${N}"
  journalctl -u "$APP" -n 30 --no-pager || true
  if [[ -x "${BIN}.prev" && -d "${DIR}.prev" ]]; then
    echo -e "${Y}Restoring the previous version automatically...${N}"
    rollback_app --yes || true
  else
    echo -e "${Y}No previous version to restore. Try: sudo systemctl restart $APP${N}"
  fi
}

change_login(){
  current=$(getv HUB_USERNAME)
  read -rp "New username [${current:-admin}]: " username </dev/tty
  username=${username:-${current:-admin}}
  read -rsp "New password (12+ characters): " p1 </dev/tty
  echo
  [[ ${#p1} -ge 12 ]] || { echo -e "${R}Password too short.${N}"; pause; return; }
  read -rsp "Confirm password: " p2 </dev/tty
  echo
  [[ "$p1" == "$p2" ]] || { echo -e "${R}Passwords do not match.${N}"; pause; return; }

  hash=$("$BIN" hash-password "$p1")
  cookie_secure=$(getv HUB_COOKIE_SECURE)
  [[ -z "$cookie_secure" ]] && cookie_secure="false"
  addr=$(getv HUB_ADDR)
  [[ -z "$addr" ]] && addr="0.0.0.0:8787"

  # Write directly via printf (never sed) so '&' and '$' characters in bcrypt hashes are never corrupted
  umask 077
  printf 'HUB_ADDR=%s\nHUB_DATA_DIR=%s\nHUB_WEB_DIR=%s/web/dist\nHUB_USERNAME=%s\nHUB_PASSWORD_HASH=%s\nHUB_COOKIE_SECURE=%s\n' \
    "$addr" "$DATA" "$DIR" "$username" "$hash" "$cookie_secure" > "$ENV_FILE"
  chmod 600 "$ENV_FILE"

  systemctl restart "$APP"
  unset p1 p2 hash
  echo -e "${G}✓ Login credentials updated cleanly for user '$username'.${N}"
  pause
}

ssl_setup(){
  read -rp "Domain pointing to this server (e.g. hub.example.com): " domain </dev/tty
  [[ -n "$domain" ]] || { pause; return; }
  read -rp "Email for certificate notices: " email </dev/tty
  if ! command -v caddy >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl gnupg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq && apt-get install -y caddy
  fi
  mkdir -p /etc/caddy
  {
    [[ -n "$email" ]] && printf "{\n email %s\n}\n" "$email"
    printf "%s {\n encode zstd gzip\n reverse_proxy 127.0.0.1:8787\n}\n" "$domain"
  } > "$CADDY"
  sed -i 's/^HUB_ADDR=.*/HUB_ADDR=127.0.0.1:8787/' "$ENV_FILE"
  sed -i 's/^HUB_COOKIE_SECURE=.*/HUB_COOKIE_SECURE=true/' "$ENV_FILE"
  systemctl enable --now caddy && systemctl reload caddy || systemctl restart caddy
  systemctl restart "$APP"
  echo -e "${G}HTTPS configured: https://$domain${N}"
  pause
}

uninstall_app(){
  echo -e "${R}This will completely remove Sahand-CorText and its service.${N}"
  read -rp "Type REMOVE to confirm: " confirm </dev/tty
  [[ "$confirm" == REMOVE ]] || { echo "Cancelled."; pause; return; }
  systemctl disable --now "$APP" 2>/dev/null || true
  rm -f "$SERVICE" "$BIN" "${BIN}.prev" /usr/local/bin/cortext "$ENV_FILE"
  rm -rf "$DIR" "${DIR}.prev" /root/.cache/go-build /root/go /tmp/go.tgz
  systemctl daemon-reload
  read -rp "Also delete database and conversation history at $DATA? [y/N]: " db </dev/tty
  [[ "$db" =~ ^[Yy]$ ]] && rm -rf "$DATA"
  echo -e "${G}Sahand-CorText uninstalled cleanly.${N}"
  exit 0
}

if [[ "${1:-}" == "update" || "${1:-}" == "--update" ]]; then
  update_app
  exit 0
fi
if [[ "${1:-}" == "rollback" || "${1:-}" == "--rollback" ]]; then
  rollback_app
  exit $?
fi

while true; do
  header
  INSTALLED_NOW=0
  cat <<EOF
  1) Update Sahand-CorText (Fast Update)
  2) Roll back to previous version
  3) Show panel address
  4) Change username and password
  5) Configure / renew SSL (HTTPS)
  6) Restart service
  7) View live logs
  8) Uninstall Sahand-CorText
  0) Exit
EOF
  read -rp $'\nSelect an option: ' choice </dev/tty
  case "$choice" in
    1) update_app; pause ;;
    2) rollback_app || true; pause ;;
    3) echo -e "\nPanel URL: ${C}$(panel_url)${N}"; pause ;;
    4) change_login ;;
    5) ssl_setup ;;
    6) systemctl restart "$APP"; systemctl --no-pager --full status "$APP"; pause ;;
    7) journalctl -u "$APP" -f ;;
    8) uninstall_app ;;
    0) exit 0 ;;
    *) echo "Invalid option."; sleep 1 ;;
  esac
done
