#!/usr/bin/env bash
set -Eeuo pipefail
APP=sahand-cortext
ENV_FILE=/etc/sahand-cortext.env
SERVICE=/etc/systemd/system/sahand-cortext.service
BIN=/usr/local/bin/sahand-cortext
DATA=/var/lib/sahand-cortext
CADDY=/etc/caddy/Caddyfile
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
  echo -e "  ${Y}STATUS     :${N} $(systemctl is-active "$APP" 2>/dev/null || echo unknown)   |   ${Y}SSH MENU :${N} sudo cortext"
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
  sed -i "s/^HUB_USERNAME=.*/HUB_USERNAME=$username/;s|^HUB_PASSWORD_HASH=.*|HUB_PASSWORD_HASH=$hash|" "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  systemctl restart "$APP"
  unset p1 p2 hash
  echo -e "${G}Login credentials updated.${N}"
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
  echo -e "${R}This removes the application, service, and local database.${N}"
  read -rp "Type REMOVE to confirm: " confirm </dev/tty
  [[ "$confirm" == REMOVE ]] || { echo "Cancelled."; pause; return; }
  systemctl disable --now "$APP" 2>/dev/null || true
  rm -f "$SERVICE" "$BIN" /usr/local/bin/cortext "$ENV_FILE"
  systemctl daemon-reload
  read -rp "Also remove database at $DATA? [y/N]: " db </dev/tty
  [[ "$db" =~ ^[Yy]$ ]] && rm -rf "$DATA"
  echo -e "${G}Sahand-CorText uninstalled.${N}"
  exit 0
}

while true; do
  header
  INSTALLED_NOW=0
  cat <<EOF
  1) Show panel address
  2) Change username and password
  3) Configure / renew SSL (HTTPS)
  4) Restart service
  5) View live logs
  6) Uninstall Sahand-CorText
  0) Exit
EOF
  read -rp $'\nSelect an option: ' choice </dev/tty
  case "$choice" in
    1) echo -e "\nPanel URL: ${C}$(panel_url)${N}"; pause ;;
    2) change_login ;;
    3) ssl_setup ;;
    4) systemctl restart "$APP"; systemctl --no-pager --full status "$APP"; pause ;;
    5) journalctl -u "$APP" -f ;;
    6) uninstall_app ;;
    0) exit 0 ;;
    *) echo "Invalid option."; sleep 1 ;;
  esac
done
