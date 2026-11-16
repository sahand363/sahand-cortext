#!/usr/bin/env python3
"""CorText Mobile relay v2 — dumb transparent proxy (Python stdlib only).

Forwards the Android app's /api/mobile/* requests to the hub (CorText)
and returns the hub's response. Holds NO token, never decrypts, only
talks to the hub. The hub verifies the app's token by decrypting the
request body; the relay just forwards the encrypted bytes.

Usage:
    python3 relay.py --hub http://<hub-ip>:8787 --port 8080

Flags:
    --hub       Hub base URL (required), e.g. http://192.168.1.5:8787
    --port      Port to listen on (default 8080)
    --host      Bind address (default 0.0.0.0)
    --tls-cert  TLS certificate file (serve HTTPS when given with --tls-key)
    --tls-key   TLS private key file

Behaviour:
    * ANY request whose path starts with /api/mobile/ is forwarded to
      <hub><path> with the same method, query, headers (plus
      X-Mobile-Relay: <relay-id>) and the exact raw body, with a 600s
      timeout. The hub's exact response (status, body, Content-Type)
      is returned. The body is never read, parsed, or decrypted.
      Streaming responses (Content-Type: text/event-stream) are passed
      through chunk-by-chunk as they arrive, instead of being buffered,
      so a streamed reply reaches the app word-by-word.
    * GET / returns plain JSON {"ok": true, "service": "cortext-relay",
      "hub": "<hub>"}.
    * Anything else returns 404 plain JSON {"error": "not found"}.
    * If the hub cannot be reached, returns 502 {"error": "hub unreachable"}.
    * Relay ID: a random 8-hex string generated at startup, logged on
      start, sent as X-Mobile-Relay on every forwarded request.
    * Each forwarded request is logged (method + path + hub status).
"""

import argparse
import http.client
import http.server
import json
import secrets
import ssl
import sys
import urllib.parse

FORWARD_TIMEOUT_SECS = 600

# Hop-by-hop headers: never forwarded in either direction.
HOP_BY_HOP = {
    "connection", "keep-alive", "proxy-connection", "te", "trailer",
    "proxy-authorization", "proxy-authenticate", "upgrade",
    "transfer-encoding", "content-length",
}


def log(msg):
    print(msg, flush=True)


class RelayHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # -- helpers ---------------------------------------------------------
    def _send_json(self, status, obj):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_body(self):
        length = self.headers.get("Content-Length")
        if length is None:
            return b""
        try:
            n = int(length)
        except ValueError:
            return b""
        if n <= 0:
            return b""
        return self.rfile.read(n)

    def _forward(self):
        """Forward this request to the hub and return the hub's response.
        Streaming responses (Content-Type: text/event-stream) are passed
        through chunk-by-chunk as they arrive, instead of being buffered;
        every other response is buffered and returned exactly as before.
        """
        hub = self.server.hub_url          # e.g. http://192.168.1.5:8787
        parsed = urllib.parse.urlparse(hub)
        is_https = parsed.scheme == "https"
        hub_host = parsed.hostname
        hub_port = parsed.port or (443 if is_https else 80)

        body = self._read_body()

        # Copy headers, dropping hop-by-hop ones; Host and
        # Content-Length are set by http.client from the target URL/body.
        headers = {}
        for name, value in self.headers.items():
            if name.lower() in HOP_BY_HOP or name.lower() == "host":
                continue
            headers[name] = value
        headers["X-Mobile-Relay"] = self.server.relay_id

        # Preserve the path and query exactly as the app sent them.
        target = self.path

        try:
            if is_https:
                conn = http.client.HTTPSConnection(
                    hub_host, hub_port, timeout=FORWARD_TIMEOUT_SECS)
            else:
                conn = http.client.HTTPConnection(
                    hub_host, hub_port, timeout=FORWARD_TIMEOUT_SECS)
            conn.request(self.command, target, body=body, headers=headers)
            resp = conn.getresponse()
            status = resp.status
            content_type = resp.getheader("Content-Type") or ""
        except Exception as exc:  # hub unreachable / timeout / TLS error
            log("FORWARD %s %s -> hub unreachable (%s)"
                % (self.command, self.path, exc))
            self._send_json(502, {"error": "hub unreachable"})
            return

        log("FORWARD %s %s -> %d" % (self.command, self.path, status))

        if content_type.startswith("text/event-stream"):
            # SSE: forward each chunk as it arrives (no Content-Length;
            # the client reads until the connection closes).
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            cache = resp.getheader("Cache-Control")
            if cache:
                self.send_header("Cache-Control", cache)
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True
            # read1 (not read): return as soon as ANY bytes are available,
            # instead of waiting to fill the buffer or for EOF — read()
            # would block until 4096 bytes or the end of the stream, which
            # is exactly the buffering this code exists to avoid.
            read_chunk = getattr(resp, "read1", resp.read)
            try:
                while True:
                    chunk = read_chunk(4096)
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    self.wfile.flush()
            except Exception as exc:
                log("STREAM %s %s -> interrupted (%s)"
                    % (self.command, self.path, exc))
            finally:
                conn.close()
            return

        # Non-streaming: buffer the whole body and return it exactly as
        # before.
        try:
            resp_body = resp.read()
        except Exception as exc:
            log("FORWARD %s %s -> read failed (%s)"
                % (self.command, self.path, exc))
            conn.close()
            self._send_json(502, {"error": "hub unreachable"})
            return
        conn.close()

        self.send_response(status)
        if content_type:
            self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(resp_body)))
        self.end_headers()
        if resp_body:
            self.wfile.write(resp_body)

    # -- request handling ------------------------------------------------
    def _handle(self):
        if self.path == "/" and self.command == "GET":
            self._send_json(200, {
                "ok": True,
                "service": "cortext-relay",
                "hub": self.server.hub_url,
            })
            return
        if self.path.startswith("/api/mobile/"):
            self._forward()
            return
        self._send_json(404, {"error": "not found"})

    def do_GET(self):
        self._handle()

    def do_POST(self):
        self._handle()

    def do_PUT(self):
        self._handle()

    def do_DELETE(self):
        self._handle()

    def do_PATCH(self):
        self._handle()

    def log_message(self, fmt, *args):  # keep stdout to our own log lines
        pass


class RelayServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def main():
    parser = argparse.ArgumentParser(
        description="CorText Mobile relay v2 — transparent proxy to the hub.")
    parser.add_argument("--hub", required=True,
                        help="Hub base URL, e.g. http://192.168.1.5:8787")
    parser.add_argument("--port", type=int, default=8080,
                        help="Port to listen on (default 8080)")
    parser.add_argument("--host", default="0.0.0.0",
                        help="Bind address (default 0.0.0.0)")
    parser.add_argument("--tls-cert", default=None,
                        help="TLS certificate file (serve HTTPS with --tls-key)")
    parser.add_argument("--tls-key", default=None,
                        help="TLS private key file")
    args = parser.parse_args()

    hub = args.hub.rstrip("/")
    if not hub.startswith(("http://", "https://")):
        print("error: --hub must start with http:// or https://", file=sys.stderr)
        sys.exit(2)
    if bool(args.tls_cert) != bool(args.tls_key):
        print("error: --tls-cert and --tls-key must be given together",
              file=sys.stderr)
        sys.exit(2)

    relay_id = secrets.token_hex(4)  # 8 hex chars

    server = RelayServer((args.host, args.port), RelayHandler)
    server.hub_url = hub
    server.relay_id = relay_id

    scheme = "http"
    if args.tls_cert and args.tls_key:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(args.tls_cert, args.tls_key)
        server.socket = ctx.wrap_socket(server.socket, server_side=True)
        scheme = "https"

    log("cortext-relay v2 up: relay id %s, forwarding to %s" % (relay_id, hub))
    log("listening on %s://%s:%d" % (scheme, args.host, args.port))
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
