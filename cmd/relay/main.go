// Command cortext-relay is the CorText Mobile relay — a dumb transparent
// proxy (Go standard library only) that forwards the Android app's
// /api/mobile/* requests to the hub (CorText) and returns the hub's
// response. It holds NO token and never decrypts; it only talks to the
// hub. The hub verifies the app's token by decrypting the request body;
// the relay just forwards the encrypted bytes.
//
// This replaces the old Python relay (internal/mobile/relay.py). The
// behaviour is identical, with one difference that matters: streaming
// (SSE) responses are forwarded chunk-by-chunk with a flush after each
// chunk. Go's response Body.Read returns as soon as ANY bytes are
// available, so a streamed reply reaches the app word-by-word — the
// Python relay had to use read1() to get the same effect, because
// resp.read() blocked until its buffer filled.
//
// Usage:
//
//	cortext-relay --hub http://<hub-ip>:8787 --port 8080
//
// Flags:
//   --hub       Hub base URL (required), e.g. http://192.168.1.5:8787
//   --port      Port to listen on (default 8080)
//   --host      Bind address (default 0.0.0.0)
//   --tls-cert  TLS certificate file (serve HTTPS when given with --tls-key)
//   --tls-key   TLS private key file
//
// Behaviour:
//   - ANY request whose path starts with /api/mobile/ is forwarded to
//     <hub><path> with the same method, query, headers (plus
//     X-Mobile-Relay: <relay-id>) and the exact raw body, with a 600s
//     timeout. The hub's exact response (status, body, Content-Type)
//     is returned. The body is never read, parsed, or decrypted.
//     Streaming responses (Content-Type: text/event-stream) are passed
//     through chunk-by-chunk as they arrive, instead of being buffered,
//     so a streamed reply reaches the app word-by-word.
//   - GET / returns plain JSON {"ok": true, "service": "cortext-relay",
//     "hub": "<hub>"}.
//   - Anything else returns 404 plain JSON {"error": "not found"}.
//   - If the hub cannot be reached, returns 502 {"error": "hub unreachable"}.
//   - Relay ID: a random 8-hex string generated at startup, logged on
//     start, sent as X-Mobile-Relay on every forwarded request.
//   - Each forwarded request is logged (method + path + hub status).
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const forwardTimeout = 600 * time.Second

// hopByHop headers are never forwarded in either direction.
var hopByHop = map[string]bool{
	"connection":           true,
	"keep-alive":           true,
	"proxy-connection":     true,
	"te":                   true,
	"trailer":              true,
	"proxy-authorization":  true,
	"proxy-authenticate":   true,
	"upgrade":              true,
	"transfer-encoding":    true,
	"content-length":       true,
}

type relay struct {
	hub      string // e.g. http://192.168.1.5:8787 (no trailing slash)
	relayID  string // random 8 hex chars
	client   *http.Client
}

func (rl *relay) writeJSON(w http.ResponseWriter, status int, obj any) {
	body, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// forward sends this request to the hub and returns the hub's response.
func (rl *relay) forward(w http.ResponseWriter, r *http.Request) {
	// Read the exact raw body (may be empty for GET).
	body, err := io.ReadAll(r.Body)
	if err != nil {
		rl.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "hub unreachable"})
		return
	}

	// Preserve the path and query exactly as the app sent them.
	target := rl.hub + r.URL.RequestURI()
	req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
	if err != nil {
		rl.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "hub unreachable"})
		return
	}
	// Copy headers, dropping hop-by-hop ones; Host and Content-Length
	// are set by the client from the target URL/body.
	for name, values := range r.Header {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("X-Mobile-Relay", rl.relayID)

	resp, err := rl.client.Do(req)
	if err != nil {
		log.Printf("FORWARD %s %s -> hub unreachable (%v)", r.Method, r.URL.RequestURI(), err)
		rl.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "hub unreachable"})
		return
	}
	defer resp.Body.Close()
	log.Printf("FORWARD %s %s -> %d", r.Method, r.URL.RequestURI(), resp.StatusCode)

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		// SSE: forward each chunk as it arrives (no Content-Length;
		// the client reads until the connection closes). Each Read
		// on resp.Body returns as soon as ANY bytes are available —
		// it never waits to fill the buffer — and we flush after
		// every write, so nothing is buffered here.
		w.Header().Set("Content-Type", contentType)
		if cache := resp.Header.Get("Cache-Control"); cache != "" {
			w.Header().Set("Cache-Control", cache)
		}
		w.Header().Set("Connection", "close")
		w.WriteHeader(resp.StatusCode)
		flusher, ok := w.(http.Flusher)
		if !ok {
			// No flusher (should not happen with net/http): fall back
			// to a plain copy.
			_, _ = io.Copy(w, resp.Body)
			return
		}
		flusher.Flush()
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					log.Printf("STREAM %s %s -> interrupted (%v)", r.Method, r.URL.RequestURI(), werr)
					return
				}
				// Flush after EVERY chunk: this is the whole point.
				flusher.Flush()
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("STREAM %s %s -> interrupted (%v)", r.Method, r.URL.RequestURI(), err)
				}
				return
			}
		}
	}

	// Non-streaming: buffer the whole body and return it exactly as
	// before.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("FORWARD %s %s -> read failed (%v)", r.Method, r.URL.RequestURI(), err)
		rl.writeJSON(w, http.StatusBadGateway, map[string]string{"error": "hub unreachable"})
		return
	}
	for name, values := range resp.Header {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(respBody)))
	w.WriteHeader(resp.StatusCode)
	if len(respBody) > 0 {
		_, _ = w.Write(respBody)
	}
}

func (rl *relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		rl.writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"service": "cortext-relay",
			"hub":     rl.hub,
		})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/mobile/") {
		rl.forward(w, r)
		return
	}
	rl.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func newRelayID() (string, error) {
	b := make([]byte, 4) // 4 bytes -> 8 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func main() {
	log.SetFlags(0)
	hub := flag.String("hub", "", "Hub base URL, e.g. http://192.168.1.5:8787")
	port := flag.Int("port", 8080, "Port to listen on")
	host := flag.String("host", "0.0.0.0", "Bind address")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (serve HTTPS with --tls-key)")
	tlsKey := flag.String("tls-key", "", "TLS private key file")
	flag.Parse()

	hubURL := strings.TrimRight(*hub, "/")
	if !strings.HasPrefix(hubURL, "http://") && !strings.HasPrefix(hubURL, "https://") {
		fmt.Fprintln(os.Stderr, "error: --hub must start with http:// or https://")
		os.Exit(2)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "error: --tls-cert and --tls-key must be given together")
		os.Exit(2)
	}

	relayID, err := newRelayID()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: could not generate relay id:", err)
		os.Exit(1)
	}

	rl := &relay{
		hub:     hubURL,
		relayID: relayID,
		client:  &http.Client{Timeout: forwardTimeout},
	}
	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", *host, *port),
		Handler: rl,
	}

	scheme := "http"
	if *tlsCert != "" && *tlsKey != "" {
		scheme = "https"
	}
	log.Printf("cortext-relay up: relay id %s, forwarding to %s", relayID, hubURL)
	log.Printf("listening on %s://%s:%d", scheme, *host, *port)
	if scheme == "https" {
		err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil {
		log.Fatal(err)
	}
}
