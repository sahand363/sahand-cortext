#!/usr/bin/env bash
# Nokhodi Chat - server management menu (TUI)
# Run:  sudo ./manage.sh
#        (or ./manage.sh when testing locally without systemd)
#
# Install dir: /opt/nokhodi-chat
#   binary:    /opt/nokhodi-chat/nokhodi-chat
#   config:    /opt/nokhodi-chat/config.json   (NEVER deleted by update)
#   messages:  /opt/nokhodi-chat/data/room_*.json (NEVER deleted by update)
#   rooms:     /opt/nokhodi-chat/data/rooms.json (NEVER deleted by update)
#   service:   nokhodi-chat.service (systemd)
set -u
# --- Input fix (v2.0.1) ---
# When this menu is launched from a pipe (e.g. `curl ... | sudo bash`
# ending in `exec manage.sh`), stdin is the pipe, not your keyboard.
# `read` then gets EOF instantly, choice is empty, the menu prints
# "Invalid choice.", clears, and loops forever - the flicker every
# few seconds. So: take input from the real terminal (/dev/tty)
# instead, and if there is no terminal at all, exit instead of looping.
if [ ! -t 0 ]; then
  # Test-open /dev/tty in a subshell first: merely existing is not
  # enough - opening it fails when there is no controlling terminal.
  if (exec 0</dev/tty) 2>/dev/null; then
    exec 0</dev/tty
  else
    echo "No terminal input available - run 'nokhodichat' from a normal SSH prompt, not from a pipe."
    exit 1
  fi
fi
REPO="sahand363/nokhodi-chat"
INSTALL_DIR="/opt/nokhodi-chat"
BIN="$INSTALL_DIR/nokhodi-chat"
CONFIG="$INSTALL_DIR/config.json"
SERVICE="nokhodi-chat"
PORT="44044"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
info()  { echo -e "${CYAN}$*${NC}"; }
ok()    { echo -e "${GREEN}$*${NC}"; }
warn()  { echo -e "${YELLOW}$*${NC}"; }
err()   { echo -e "${RED}$*${NC}"; }
pause() { read -rp "Press Enter to return to the menu..." _; }

server_ip() {
  hostname -I 2>/dev/null | awk '{print $1}'
}

installed_version() {
  if [ -x "$BIN" ]; then "$BIN" version 2>/dev/null || echo "unknown"; else echo "not installed"; fi
}

# --- SSL / TLS helpers (v2.1.0) ---
# SSL is optional and toggled from this menu. When it is on, the same
# port (44044) serves HTTPS instead of HTTP; when it is off, plain HTTP
# works again. Certificates live in $INSTALL_DIR/tls/ (self-signed) or
# in /etc/letsencrypt/live/<domain>/ (Let's Encrypt).
TLS_DIR="$INSTALL_DIR/tls"

tls_enabled() { [ -f "$CONFIG" ] && grep -q '"tls_enabled": *true' "$CONFIG"; }
tls_mode()    { [ -f "$CONFIG" ] && grep -o '"tls_mode": *"[^"]*"' "$CONFIG" | sed 's/.*: *"//; s/"$//' || echo ""; }
tls_domain()  { [ -f "$CONFIG" ] && grep -o '"tls_domain": *"[^"]*"' "$CONFIG" | sed 's/.*: *"//; s/"$//' || echo ""; }
tls_cert()    { [ -f "$CONFIG" ] && grep -o '"tls_cert_file": *"[^"]*"' "$CONFIG" | sed 's/.*: *"//; s/"$//' || echo ""; }

scheme() { if tls_enabled; then echo "https"; else echo "http"; fi; }

# Secret front door (v2.3): when config.json has a secret_path, the
# chat only opens at /<secret_path> and "/" returns 404. The main
# chat link shown in this menu is always the real link, so the OP
# can never lock themself out.
secret_path() { [ -f "$CONFIG" ] && grep -o '"secret_path": *"[^"]*"' "$CONFIG" | sed 's/.*: *"//; s/"$//' || echo ""; }
secret_status_line() {
  local sp; sp="$(secret_path)"
  if [ -n "$sp" ]; then echo "Front door: SECRET ON (only the link below works)"; else echo "Front door: OFF (chat opens at /)"; fi
}
chat_link() {
  local sp; sp="$(secret_path)"
  if [ -n "$sp" ]; then echo "$(scheme)://$(server_ip):$PORT/$sp"; else echo "$(scheme)://$(server_ip):$PORT"; fi
}

restart_service() {
  if has_systemd; then
    systemctl restart "$SERVICE" 2>/dev/null || systemctl start "$SERVICE" 2>/dev/null || true
  else
    pkill -f "$BIN" 2>/dev/null || true
    (cd "$INSTALL_DIR" && nohup "$BIN" -config "$CONFIG" >>"$INSTALL_DIR/chat.log" 2>&1 &)
    sleep 1
  fi
}

ssl_status_line() {
  if tls_enabled; then
    local cert expiry
    cert="$(tls_cert)"
    expiry="unknown"
    [ -n "$cert" ] && [ -f "$cert" ] && expiry="$(openssl x509 -in "$cert" -noout -enddate 2>/dev/null | sed 's/notAfter=//')"
    echo "SSL: ON ($(tls_mode), domain: $(tls_domain), expires: $expiry)"
  else
    echo "SSL: OFF (plain HTTP)"
  fi
}

do_ssl_selfsigned() {
  echo; info "== Enable SSL - Self-Signed Certificate =="
  info "No domain needed. Works with your server IP - but browsers will"
  info "show a warning (\"not secure / proceed anyway\") because the"
  info "certificate is not signed by a public authority. That is normal."
  command -v openssl >/dev/null || { info "Installing openssl..."; apt-get update -qq && apt-get install -y -qq openssl || { err "Could not install openssl."; pause; return; }; }
  read -rp "Domain name (optional - press Enter to use the server IP only): " domain
  local ip san cn
  ip="$(server_ip)"
  cn="${domain:-$ip}"
  san="IP:$ip,IP:127.0.0.1,DNS:localhost"
  [ -n "$domain" ] && san="DNS:$domain,$san"
  mkdir -p "$TLS_DIR"
  info "Generating a 365-day certificate for $cn ..."
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$TLS_DIR/key.pem" -out "$TLS_DIR/cert.pem" \
    -days 365 -subj "/CN=$cn" -addext "subjectAltName=$san" \
    || { err "Certificate generation failed."; pause; return; }
  chmod 600 "$TLS_DIR/key.pem"; chmod 644 "$TLS_DIR/cert.pem"
  "$BIN" tls -config "$CONFIG" -enable true -mode selfsigned \
    -domain "$domain" -cert "$TLS_DIR/cert.pem" -key "$TLS_DIR/key.pem" \
    || { err "Failed to enable TLS in config."; pause; return; }
  restart_service
  ok "SSL is ON (self-signed). Open: https://$cn:$PORT"
  warn "First visit: your browser will warn about the certificate - click Advanced -> Proceed. That only happens because it is self-signed."
  info "Expires: $(openssl x509 -in "$TLS_DIR/cert.pem" -noout -enddate 2>/dev/null | sed 's/notAfter=//')"
  pause
}

do_ssl_letsencrypt() {
  echo; info "== Enable SSL - Let's Encrypt (trusted certificate) =="
  info "Needs a domain name that already points to this server (DNS A"
  info "record -> $(server_ip)). The certificate is free, trusted by"
  info "browsers (no warning), and renews itself automatically."
  read -rp "Domain name (e.g. chat.example.com): " domain
  [ -n "$domain" ] || { err "A domain name is required for Let's Encrypt."; pause; return; }
  command -v certbot >/dev/null || { info "Installing certbot..."; apt-get update -qq && apt-get install -y -qq certbot || { err "Could not install certbot."; pause; return; }; }
  command -v ufw >/dev/null && ufw allow 80/tcp >/dev/null 2>&1 || true
  if command -v ss >/dev/null && ss -tln 2>/dev/null | grep -q ':80 '; then
    warn "Something is already listening on port 80 - certbot needs port 80 free to prove you own the domain."
    warn "Stop that service first, or use the self-signed option instead."
    pause; return
  fi
  info "Asking Let's Encrypt for a certificate for $domain ... (this needs port 80 open)"
  certbot certonly --standalone --non-interactive --agree-tos \
    --register-unsafely-without-email -d "$domain" \
    || { err "Let's Encrypt failed - check that $domain really points to $(server_ip) and port 80 is reachable."; pause; return; }
  # Restart the chat when certbot renews the certificate.
  mkdir -p /etc/letsencrypt/renewal-hooks/deploy
  cat > /etc/letsencrypt/renewal-hooks/deploy/nokhodi-chat.sh <<'EOF'
#!/bin/sh
systemctl restart nokhodi-chat 2>/dev/null || true
EOF
  chmod +x /etc/letsencrypt/renewal-hooks/deploy/nokhodi-chat.sh
  "$BIN" tls -config "$CONFIG" -enable true -mode letsencrypt \
    -domain "$domain" \
    -cert "/etc/letsencrypt/live/$domain/fullchain.pem" \
    -key "/etc/letsencrypt/live/$domain/privkey.pem" \
    || { err "Failed to enable TLS in config."; pause; return; }
  restart_service
  ok "SSL is ON (Let's Encrypt). Open: https://$domain:$PORT"
  info "The certificate renews automatically (certbot timer) and the chat restarts on renewal."
  pause
}

do_ssl_disable() {
  echo; info "== Disable SSL (back to plain HTTP) =="
  info "The certificate files are kept on disk - you can turn SSL back on"
  info "again without generating a new certificate."
  "$BIN" tls -config "$CONFIG" -enable false || { err "Failed to disable TLS."; pause; return; }
  restart_service
  ok "SSL is OFF. Open: http://$(server_ip):$PORT"
  pause
}

do_ssl_delete() {
  echo; info "== Delete SSL Certificate =="
  warn "This deletes the certificate files and turns SSL off."
  read -rp "Are you sure? [y/N]: " confirm
  [[ "$confirm" =~ ^[Yy]$ ]] || { info "Cancelled."; pause; return; }
  local domain mode
  domain="$(tls_domain)"; mode="$(tls_mode)"
  "$BIN" tls -config "$CONFIG" -clear 2>/dev/null || true
  rm -rf "$TLS_DIR"
  if [ "$mode" = "letsencrypt" ] && [ -n "$domain" ] && command -v certbot >/dev/null; then
    certbot delete --cert-name "$domain" --non-interactive 2>/dev/null || true
    rm -f /etc/letsencrypt/renewal-hooks/deploy/nokhodi-chat.sh
  fi
  restart_service
  ok "SSL certificate deleted. The chat is back on plain HTTP: http://$(server_ip):$PORT"
  pause
}

do_ssl_reissue() {
  echo; info "== Delete & Get a New Certificate =="
  info "This deletes the current certificate, then makes a new one."
  read -rp "Are you sure? [y/N]: " confirm
  [[ "$confirm" =~ ^[Yy]$ ]] || { info "Cancelled."; pause; return; }
  local domain mode
  domain="$(tls_domain)"; mode="$(tls_mode)"
  "$BIN" tls -config "$CONFIG" -clear 2>/dev/null || true
  rm -rf "$TLS_DIR"
  if [ "$mode" = "letsencrypt" ] && [ -n "$domain" ] && command -v certbot >/dev/null; then
    certbot delete --cert-name "$domain" --non-interactive 2>/dev/null || true
  fi
  ok "Old certificate deleted. Now choose the new one:"
  echo "  1) Self-signed (no domain needed, browser will warn)"
  echo "  2) Let's Encrypt (needs a domain, no browser warning)"
  read -rp "Choose [1-2]: " kind
  case "$kind" in
    1) do_ssl_selfsigned ;;
    2) do_ssl_letsencrypt ;;
    *) warn "Cancelled - SSL is off for now."; pause ;;
  esac
}

do_secret_menu() {
  while true; do
    clear
    echo -e "${GREEN}+------------------------------------------+${NC}"
    echo -e "${GREEN}|     Nokhodi Chat - Secret Front Door    |${NC}"
    echo -e "${GREEN}+------------------------------------------+${NC}"
    echo -e "  $(secret_status_line)"
    echo -e "  Chat link: ${CYAN}$(chat_link)${NC}"
    echo "  (When the secret door is ON, visiting just"
    echo "   http://<ip>:44044/ shows nothing - only this"
    echo "   link opens the chat. The login password is"
    echo "   still asked for. This menu always shows the"
    echo "   current link, so you can never lock yourself out.)"
    echo
    echo "  1) Enable secret front door (random link)"
    echo "  2) Disable (back to plain /)"
    echo "  3) New random link (the old one stops working)"
    echo "  0) Back to main menu"
    echo
    if ! read -rp "Choose [0-3]: " choice; then
      echo; echo "Input closed - exiting menu."
      exit 0
    fi
    case "$choice" in
      1) "$BIN" secret -config "$CONFIG" -enable true && { ok "Secret front door enabled."; info "Restarting the service..."; restart_service; info "Chat link: $(chat_link)"; } || err "Failed to enable."; pause ;;
      2) "$BIN" secret -config "$CONFIG" -enable false && { ok "Secret front door disabled."; info "Restarting the service..."; restart_service; } || err "Failed to disable."; pause ;;
      3) "$BIN" secret -config "$CONFIG" -regenerate && { ok "New secret link generated."; info "Restarting the service..."; restart_service; info "New chat link: $(chat_link)"; } || err "Failed to regenerate."; pause ;;
      0) return ;;
      *) warn "Invalid choice."; sleep 1 ;;
    esac
  done
}

do_ssl_menu() {
  while true; do
    clear
    echo -e "${GREEN}+------------------------------------------+${NC}"
    echo -e "${GREEN}|     Nokhodi Chat - SSL / TLS      |${NC}"
    echo -e "${GREEN}+------------------------------------------+${NC}"
    echo -e "  $(ssl_status_line)"
    echo -e "  URL: ${CYAN}$(scheme)://$(server_ip):$PORT${NC}"
    echo
    echo "  1) Enable SSL - Self-signed (no domain needed, browser will warn)"
    echo "  2) Enable SSL - Let's Encrypt (needs a domain, no warning)"
    echo "  3) Disable SSL (back to plain HTTP - keeps the certificate)"
    echo "  4) Delete SSL certificate (removes it + turns SSL off)"
    echo "  5) Delete & get a new certificate (re-issue)"
    echo "  0) Back to main menu"
    echo
    if ! read -rp "Choose [0-5]: " choice; then
      echo; echo "Input closed - exiting menu."
      exit 0
    fi
    case "$choice" in
      1) do_ssl_selfsigned ;;
      2) do_ssl_letsencrypt ;;
      3) do_ssl_disable ;;
      4) do_ssl_delete ;;
      5) do_ssl_reissue ;;
      0) return ;;
      *) warn "Invalid choice."; sleep 1 ;;
    esac
  done
}

has_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }

latest_release_url() {
  # Prints the browser_download_url of the linux-amd64 tarball of the latest release.
  curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    grep -o '"browser_download_url": *"[^"]*linux-amd64\.tar\.gz"' | head -1 |
    sed 's/.*"browser_download_url": *"//; s/"$//'
}

download_binary() {
  local url tmp
  url="$(latest_release_url)"
  [ -n "$url" ] || { err "Could not find a release for $REPO - did you push a tag (v1.0.0) yet?"; return 1; }
  info "Downloading $url"
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/release.tar.gz" "$url" || { err "Download failed."; rm -rf "$tmp"; return 1; }
  tar -xzf "$tmp/release.tar.gz" -C "$tmp" || { err "Extract failed."; rm -rf "$tmp"; return 1; }
  mkdir -p "$INSTALL_DIR"
  # Keep a rollback copy of the old binary.
  [ -f "$BIN" ] && cp -f "$BIN" "$BIN.bak"
  cp -f "$tmp/nokhodi-chat" "$BIN"
  chmod +x "$BIN"
  # Ship the fresh manage.sh / service file alongside, if the release has them.
  [ -f "$tmp/manage.sh" ] && cp -f "$tmp/manage.sh" "$INSTALL_DIR/manage.sh" && chmod +x "$INSTALL_DIR/manage.sh"
  [ -f "$tmp/nokhodi-chat.service" ] && cp -f "$tmp/nokhodi-chat.service" "/etc/systemd/system/nokhodi-chat.service" 2>/dev/null || true
  # Keep the `nokhodichat` shortcut installed, too.
  if [ -f "$tmp/nokhodichat" ]; then cp -f "$tmp/nokhodichat" /usr/local/bin/nokhodichat 2>/dev/null || true; fi
  [ -f /usr/local/bin/nokhodichat ] && chmod +x /usr/local/bin/nokhodichat 2>/dev/null || true
  # Rename the title in an existing config, if it still has the old name.
  rm -rf "$tmp"
  ok "Binary updated: $($BIN version 2>/dev/null || echo ok)"
}

do_install_start() {
  echo; info "== Install / Start Service =="
  if [ ! -f "$BIN" ]; then
    info "No binary yet - downloading the latest release..."
    download_binary || { pause; return; }
  fi
  mkdir -p "$INSTALL_DIR/data"
  if [ ! -f "$CONFIG" ]; then
    # First run creates config.json with user changeme123 / OP changeme-op123.
    (cd "$INSTALL_DIR" && "$BIN" version >/dev/null 2>&1 || true)
    info "No config yet. Starting once to generate it, then set your passwords:"
    (cd "$INSTALL_DIR" && timeout 3 "$BIN" -config "$CONFIG" >/dev/null 2>&1 || true)
    do_set_credentials user
    do_set_credentials op
  fi
  if has_systemd; then
    [ -f /etc/systemd/system/nokhodi-chat.service ] || { cp -f "$(dirname "$0")/nokhodi-chat.service" /etc/systemd/system/nokhodi-chat.service 2>/dev/null || cp -f "$INSTALL_DIR/nokhodi-chat.service" /etc/systemd/system/nokhodi-chat.service; }
    systemctl daemon-reload
    systemctl enable --now "$SERVICE"
    systemctl status "$SERVICE" --no-pager | head -20
  else
    warn "No systemd here - starting in the background with nohup."
    pkill -f "$BIN" 2>/dev/null || true
    (cd "$INSTALL_DIR" && nohup "$BIN" -config "$CONFIG" >>"$INSTALL_DIR/chat.log" 2>&1 &)
    sleep 1
  fi
  ok "Chat is up at http://$(server_ip):$PORT"
  ok "Health: $(curl -fsS "http://127.0.0.1:$PORT/health" 2>/dev/null || echo 'check the log')"
  pause
}

do_update() {
  echo; info "== Update (keeps config.json + data/ - messages are NOT wiped) =="
  [ -f "$BIN" ] || { err "Nothing installed yet - use option 1 first."; pause; return; }
  warn "Current: $($BIN version)"
  download_binary || { pause; return; }
  if has_systemd; then
    systemctl restart "$SERVICE"
    systemctl status "$SERVICE" --no-pager | head -12
  else
    pkill -f "$BIN" 2>/dev/null || true
    (cd "$INSTALL_DIR" && nohup "$BIN" -config "$CONFIG" >>"$INSTALL_DIR/chat.log" 2>&1 &)
    sleep 1
  fi
  ok "Updated. Messages and rooms in $INSTALL_DIR/data/ were left untouched."
  curl -fsS "http://127.0.0.1:$PORT/health" && echo
  pause
}

do_set_credentials() {
  local role="${1:-user}"
  if [ "$role" = "op" ]; then
    echo; info "== Set / Change OP (admin) Password =="
    info "Only you use this password. It makes you the OP in the chat."
  else
    role="user"
    echo; info "== Set / Change User Password (everyone else) =="
    info "Everyone else logs in with this password. Stored as a salted hash in config.json."
  fi
  read -rsp "New $role password (min 6 chars): " pass; echo
  read -rsp "Repeat password: " pass2; echo
  [ "$pass" = "$pass2" ] || { err "Passwords do not match."; pause; return; }
  [ "${#pass}" -ge 6 ] || { err "Password must be at least 6 characters."; pause; return; }
  if [ ! -f "$BIN" ]; then
    err "Binary not found at $BIN - install first (option 1)."
    pause; return
  fi
  # Create a config first if none exists (binary creates the default one).
  if [ ! -f "$CONFIG" ]; then
    mkdir -p "$INSTALL_DIR"
    (cd "$INSTALL_DIR" && timeout 3 "$BIN" -config "$CONFIG" >/dev/null 2>&1 || true)
  fi
  "$BIN" passwd -config "$CONFIG" -role "$role" -pass "$pass" || { err "Failed to update credentials."; pause; return; }
  ok "$role password updated."
  if [ "$role" = "user" ]; then ok "Tell your users the new shared login."; fi
  if has_systemd && systemctl is-active --quiet "$SERVICE"; then
    info "Restarting the service so the new login takes effect..."
    systemctl restart "$SERVICE"
  fi
  pause
}

do_status() {
  echo; info "== Status =="
  echo "Install dir: $INSTALL_DIR"
  echo "Binary:      $([ -f "$BIN" ] && "$BIN" version || echo 'not installed')"
  echo "Config:      $([ -f "$CONFIG" ] && echo exists || echo missing)"
  echo "Messages:    $(grep -h -c '"id"' "$INSTALL_DIR"/data/room_*.json 2>/dev/null | awk '{s+=$1} END {print s+0}') stored"
  echo "Rooms:       $([ -f "$INSTALL_DIR/data/rooms.json" ] && grep -c '"id"' "$INSTALL_DIR/data/rooms.json" || echo 0)"
  echo "Chat link:   $(chat_link)"
  echo "Secret door: $(secret_status_line)"
  echo "SSL:         $(ssl_status_line)"
  if has_systemd; then systemctl status "$SERVICE" --no-pager | head -20
  else curl -fsS "http://127.0.0.1:$PORT/health" || echo "not running"; echo; fi
  pause
}

do_uninstall() {
  echo; info "== Uninstall =="
  warn "This stops the service and removes the program files."
  read -rp "Keep messages + config (data/ and config.json)? [Y/n]: " keep
  keep="${keep:-Y}"
  if has_systemd; then
    systemctl stop "$SERVICE" 2>/dev/null || true
    systemctl disable "$SERVICE" 2>/dev/null || true
    rm -f /etc/systemd/system/nokhodi-chat.service
    systemctl daemon-reload
  else
    pkill -f "$BIN" 2>/dev/null || true
  fi
  # The `nokhodichat` shortcut goes away with the program, either way.
  rm -f /usr/local/bin/nokhodichat
  if [[ "$keep" =~ ^[Yy]$ ]]; then
    mkdir -p "$INSTALL_DIR/keep"
    [ -f "$CONFIG" ] && cp -f "$CONFIG" "$INSTALL_DIR/keep-config.json.bak"
    rm -f "$BIN" "$BIN.bak"
    ok "Removed the program. Kept: config.json and data/ in $INSTALL_DIR"
  else
    rm -rf "$INSTALL_DIR"
    ok "Removed everything, including messages and config."
  fi
  pause
}

while true; do
  clear
  echo -e "${GREEN}+------------------------------------------+${NC}"
  echo -e "${GREEN}|     Nokhodi Chat - Management    |${NC}"
  echo -e "${GREEN}+------------------------------------------+${NC}"
  echo -e "  Chat link: ${CYAN}$(chat_link)${NC}"
  echo -e "  Dir: $INSTALL_DIR"
  echo -e "  Version: $(installed_version)"
  echo -e "  $(ssl_status_line)"
  echo -e "  $(secret_status_line)"
  echo
  echo "  1) Install / Start Service"
  echo "  2) Update (latest release - keeps messages, rooms & config)"
  echo "  3) Set User Password (everyone else)"
  echo "  4) Set OP Password (only you - the admin)"
  echo "  5) Status"
  echo "  6) Uninstall"
  echo "  7) SSL - Enable / Disable / Delete / Re-issue"
  echo "  8) Secret Front Door - hide the chat behind a random link"
  echo "  0) Exit"
  echo
  if ! read -rp "Choose [0-8]: " choice; then
    echo; echo "Input closed - exiting menu."
    exit 0
  fi
  case "$choice" in
    1) do_install_start ;;
    2) do_update ;;
    3) do_set_credentials user ;;
    4) do_set_credentials op ;;
    5) do_status ;;
    6) do_uninstall ;;
    7) do_ssl_menu ;;
    8) do_secret_menu ;;
    0) echo "Bye"; exit 0 ;;
    *) warn "Invalid choice."; sleep 1 ;;
  esac
done
