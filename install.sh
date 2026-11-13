#!/usr/bin/env bash
# Nokhodi Chat - one-line installer for Ubuntu
#
#   curl -sSL https://raw.githubusercontent.com/sahand363/nokhodi-chat/main/install.sh | sudo bash
#
# What it does (no compilation on the server):
#   1. Downloads the pre-built linux/amd64 binary from the latest GitHub release
#   2. Installs it to /opt/nokhodi-chat, creates config.json + data/
#   3. Installs + starts the systemd service on port 44044
#   4. Installs the `nokhodichat` command (/usr/local/bin/nokhodichat) to open the menu
#   5. Opens the management menu (./manage.sh) so you can set the OP + user passwords
set -euo pipefail
REPO="sahand363/nokhodi-chat"
INSTALL_DIR="/opt/nokhodi-chat"
PORT="44044"

echo "Installing Nokhodi Chat ..."
[ "$(id -u)" = "0" ] || { echo "Please run as root / with sudo."; exit 1; }
command -v curl >/dev/null || { apt-get update -qq && apt-get install -y -qq curl ca-certificates; }

URL="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
  grep -o '"browser_download_url": *"[^"]*linux-amd64\.tar\.gz"' | head -1 |
  sed 's/.*"browser_download_url": *"//; s/"$//')"
[ -n "$URL" ] || { echo "ERROR: no release found for $REPO - push a tag like v1.0.0 first."; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/release.tar.gz" "$URL"
tar -xzf "$TMP/release.tar.gz" -C "$TMP"
mkdir -p "$INSTALL_DIR/data"
cp -f "$TMP/nokhodi-chat" "$INSTALL_DIR/nokhodi-chat"
chmod +x "$INSTALL_DIR/nokhodi-chat"
[ -f "$TMP/manage.sh" ] && cp -f "$TMP/manage.sh" "$INSTALL_DIR/manage.sh" && chmod +x "$INSTALL_DIR/manage.sh"
[ -f "$TMP/nokhodi-chat.service" ] && cp -f "$TMP/nokhodi-chat.service" /etc/systemd/system/nokhodi-chat.service
# `nokhodichat` shortcut: type it in any SSH session to open the menu.
if [ -f "$TMP/nokhodichat" ]; then
  cp -f "$TMP/nokhodichat" /usr/local/bin/nokhodichat
else
  cat > /usr/local/bin/nokhodichat <<'EOF'
#!/usr/bin/env bash
if [ "$(id -u)" = "0" ]; then exec /opt/nokhodi-chat/manage.sh "$@"; else exec sudo /opt/nokhodi-chat/manage.sh "$@"; fi
EOF
fi
chmod +x /usr/local/bin/nokhodichat
# Existing installs may still have the old title in config.json - rename it.

# First run generates config.json (defaults: user changeme123, OP changeme-op123) if missing.
if [ ! -f "$INSTALL_DIR/config.json" ]; then
  (cd "$INSTALL_DIR" && timeout 3 ./nokhodi-chat -config config.json >/dev/null 2>&1 || true)
fi

systemctl daemon-reload
systemctl enable --now nokhodi-chat
sleep 1
IP="$(hostname -I | awk '{print $1}')"
echo
SCHEME="http"
grep -q '"tls_enabled": *true' "$INSTALL_DIR/config.json" 2>/dev/null && SCHEME="https"
echo " Done! Open:  $SCHEME://$IP:$PORT"
echo "   Installed: $($INSTALL_DIR/nokhodi-chat version)"
echo "   Default passwords: user changeme123, OP changeme-op123 - change both now in the menu (options 3 and 4)."
echo "   Manage later with:  nokhodichat   (or sudo /opt/nokhodi-chat/manage.sh)"
echo
# Open the menu on the real terminal, not on the curl pipe this script
# was run from - otherwise the menu reads EOF and loops "Invalid choice."
if [ -r /dev/tty ] && [ -w /dev/tty ]; then
  exec "$INSTALL_DIR/manage.sh" < /dev/tty
else
  echo "No terminal to open the menu in - run 'nokhodichat' yourself from a normal SSH prompt."
  exit 0
fi
