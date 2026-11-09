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
  echo "http://${ip:-SERVER-IP}:8787"
}
pause(){ echo; read -rp "Press Enter to return to menu…"; }
header(){ clear; echo -e "${C}"; cat <<'EOF'
   _____                 _   _      ____            _            _
  / ____|               | | | |    / __ \          | |          | |
 | (___   __ _ _ __   __| | | |   | |  | |_ __ ___ | |_ _____  _| |_
  \___ \ / _` | '_ \ / _` | | |   | |  | | '__/ _ \| __/ _ \ \/ / __|
  ____) | (_| | | | | (_| | | |___| |__| | | | (_) | ||  __/>  <| |_
 |_____/ \__,_|_| |_|\__,_| |______\____/|_|  \___/ \__\___/_/\_\\__|
EOF
 echo -e "${N}"; echo -e "  ${G}SAHAND-CORTEXT CONTROL CENTER${N}"; echo -e "  Panel: ${C}$(panel_url)${N}"; echo "  Service: $(systemctl is-active "$APP" 2>/dev/null || echo unknown)"; echo; }
change_login(){
  current=$(getv HUB_USERNAME); read -rp "New username [${current:-admin}]: " username; username=${username:-${current:-admin}}
  read -rsp "New password (12+ characters): " p1; echo
  [[ ${#p1} -ge 12 ]] || { echo -e "${R}Password too short.${N}"; pause; return; }
  read -rsp "Confirm password: " p2; echo
  [[ "$p1" == "$p2" ]] || { echo -e "${R}Passwords do not match.${N}"; pause; return; }
  hash=$("$BIN" hash-password "$p1")
  sed -i "s/^HUB_USERNAME=.*/HUB_USERNAME=$username/;s|^HUB_PASSWORD_HASH=.*|HUB_PASSWORD_HASH=$hash|" "$ENV_FILE"
  chmod 600 "$ENV_FILE"; systemctl restart "$APP"; unset p1 p2 hash
  echo -e "${G}Login credentials updated.${N}"; pause
}
ssl_setup(){
  read -rp "Domain pointing to this server (e.g. hub.example.com): " domain
  [[ -n "$domain" ]] || { pause; return; }
  read -rp "Email for certificate notices: " email
  if ! command -v caddy >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl gnupg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq && apt-get install -y caddy
  fi
  mkdir -p /etc/caddy
  { [[ -n "$email" ]] && printf "{\n email %s\n}\n" "$email"; printf "%s {\n encode zstd gzip\n reverse_proxy 127.0.0.1:8787\n}\n" "$domain"; } > "$CADDY"
  sed -i 's/^HUB_COOKIE_SECURE=.*/HUB_COOKIE_SECURE=true/' "$ENV_FILE"
  systemctl enable --now caddy && systemctl reload caddy || systemctl restart caddy
  systemctl restart "$APP"
  echo -e "${G}HTTPS configured: https://$domain${N}"; pause
}
uninstall_app(){
  echo -e "${R}This removes the application, service, and local database.${N}"
  read -rp "Type REMOVE to confirm: " confirm
  [[ "$confirm" == REMOVE ]] || { echo "Cancelled."; pause; return; }
  systemctl disable --now "$APP" 2>/dev/null || true
  rm -f "$SERVICE" "$BIN" /usr/local/bin/cortext "$ENV_FILE"
  systemctl daemon-reload
  read -rp "Also remove database at $DATA? [y/N]: " db
  [[ "$db" =~ ^[Yy]$ ]] && rm -rf "$DATA"
  echo -e "${G}Sahand-Cortext uninstalled.${N}"; exit 0
}
while true; do
 header
 cat <<EOF
  1) Show panel address
  2) Change username and password
  3) Configure / renew SSL (HTTPS)
  4) Restart service
  5) View live logs
  6) Uninstall Sahand-Cortext
  0) Exit
EOF
 read -rp $'\nSelect an option: ' choice
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
