// Command nokhodi-chat is a small, self-hosted Persian chat room — v2.
//
//   - Two entry passwords: OP (admin) and everyone else (user).
//     The password you log in with decides your role; only the OP can
//     create rooms, set room passwords, set self-destruct timers,
//     delete any message, or kick/ban.
//   - mIRC-style rooms: #general plus rooms the OP creates. A room can
//     have its own password and a self-destruct timer, and it can be
//     hidden: hidden rooms are not listed, are only reachable via a
//     secret link, still need the room password, and return 404 —
//     no name, no messages — to anyone without link + password.
//   - Then each visitor picks their own display name + solves a maths captcha
//   - Messages are stored persistently per room in data/room_<id>.json
//   - Live updates use Server-Sent Events (SSE) — no external Go deps
//   - Serves on 0.0.0.0:44044 by default: http://<server-ip>:44044
//
// Standard library only, so `go build` works offline with no modules to fetch.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

//go:embed web
var webFS embed.FS

var version = "2.3.1"

// Config is the on-disk schema for config.json. Passwords are NEVER
// stored in plain text — only salted SHA-256 hashes.
// Legacy fields PasswordHash/Salt are the v1 shared password; on load
// they are migrated to the user (everyone) password.
type Config struct {
	Port         int    `json:"port"`
	Host         string `json:"host"`
	Username     string `json:"username,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
	Salt         string `json:"salt,omitempty"`
	// v2: two entry passwords.
	UserPasswordHash  string `json:"user_password_hash,omitempty"`
	UserSalt          string `json:"user_salt,omitempty"`
	AdminPasswordHash string `json:"admin_password_hash,omitempty"`
	AdminSalt         string `json:"admin_salt,omitempty"`
	SessionDays       int    `json:"session_days"`
	DataDir           string `json:"data_dir"`
	Title             string `json:"title"`
	// v2.1: optional SSL/TLS. Off by default — plain HTTP keeps working.
	// When on, the same port serves HTTPS instead of HTTP.
	TLSEnabled  bool   `json:"tls_enabled"`
	TLSMode     string `json:"tls_mode,omitempty"` // "selfsigned" or "letsencrypt"
	TLSDomain   string `json:"tls_domain,omitempty"`
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`
	// v2.2: uploads — images and files, with per-user limits.
	// MaxUploadMB is the largest single file (MB). Per user: at most
	// MaxUploadsPerUserPerWeek files per week, at most
	// MaxUploadBytesPerUserPerDay bytes per day, and at most
	// MaxUploadBytesPerUserTotal bytes stored on disk at once
	// (older files are freed when their messages expire or are deleted).
	MaxUploadMB                 int   `json:"max_upload_mb"`
	MaxUploadsPerUserPerWeek    int   `json:"max_uploads_per_user_per_week"`
	MaxUploadBytesPerUserPerDay int64 `json:"max_upload_bytes_per_user_per_day"`
	MaxUploadBytesPerUserTotal  int64 `json:"max_upload_bytes_per_user_total"`
	// v2.3: secret front door. Empty = off, the chat opens at "/".
	// When set, "/" returns 404 and the chat only opens at
	// "/<SecretPath>" — the menu always shows the current link, so
	// the OP can never lock themself out.
	SecretPath string `json:"secret_path,omitempty"`
}

// newRandomCode makes an unguessable random code like "shdvbfsbd4r4t"
// (lowercase letters + digits) for private room links and the secret
// front door. Used for links, never for passwords.
func newRandomCode(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// scheme returns "https" when TLS is on, "http" otherwise.
func (c *Config) scheme() string {
	if c.TLSEnabled {
		return "https"
	}
	return "http"
}

func defaultConfig() Config {
	return Config{
		Port:        44044,
		Host:        "0.0.0.0",
		SessionDays: 7,
		DataDir:     "data",
		Title:       "اتاق نخودی",
		// Upload limits (v2.2.2): 50 MB per file, 5 files per user
		// per week, 150 MB per user per day, 200 MB stored per user.
		MaxUploadMB:                 50,
		MaxUploadsPerUserPerWeek:    5,
		MaxUploadBytesPerUserPerDay: 150 * 1024 * 1024,
		MaxUploadBytesPerUserTotal:  200 * 1024 * 1024,
	}
}

func newSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newTokenHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashPassword(password, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + password))
	return hex.EncodeToString(sum[:])
}

func hashMatches(password, salt, hash string) bool {
	if hash == "" || salt == "" || password == "" {
		return false
	}
	return hashPassword(password, salt) == hash
}

// checkRole returns "op" for the OP password, "user" for the
// everyone-else password, "" for neither. OP is checked first so the
// two passwords must be different to be useful.
func checkRole(cfg *Config, password string) string {
	if hashMatches(password, cfg.AdminSalt, cfg.AdminPasswordHash) {
		return "op"
	}
	if hashMatches(password, cfg.UserSalt, cfg.UserPasswordHash) {
		return "user"
	}
	// Legacy v1 fallback: the old shared password is the user password.
	if hashMatches(password, cfg.Salt, cfg.PasswordHash) {
		return "user"
	}
	return ""
}

func setPassword(cfg *Config, role, password string) error {
	salt, err := newSalt()
	if err != nil {
		return err
	}
	h := hashPassword(password, salt)
	switch role {
	case "op", "admin":
		cfg.AdminSalt, cfg.AdminPasswordHash = salt, h
	case "user":
		cfg.UserSalt, cfg.UserPasswordHash = salt, h
		// Keep the legacy fields in sync so old tooling still sees a password.
		cfg.Salt, cfg.PasswordHash = salt, h
	default:
		return fmt.Errorf("unknown role %q (want op or user)", role)
	}
	return nil
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run: user password changeme123, OP password changeme-op123.
			// The operator MUST change both via `nokhodichat` menu 3 and 4.
			if err := setPassword(&cfg, "user", "changeme123"); err != nil {
				return nil, err
			}
			if err := setPassword(&cfg, "op", "changeme-op123"); err != nil {
				return nil, err
			}
			if err := saveConfig(path, &cfg); err != nil {
				return nil, err
			}
			log.Printf("created %s — defaults: user changeme123, OP changeme-op123 — change both with nokhodichat (options 3 and 4)", path)
			return &cfg, nil
		}
		return nil, err
	}
	// Zero the upload limits before unmarshal: cfg starts as
	// defaultConfig(), so without this an old config file that has no
	// weekly limit would still show Week=5 (the default) and the
	// v2.2.2 migration below would never trigger.
	cfg.MaxUploadMB, cfg.MaxUploadsPerUserPerWeek, cfg.MaxUploadBytesPerUserPerDay, cfg.MaxUploadBytesPerUserTotal = 0, 0, 0, 0
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// Migrate v1: old shared password becomes the user password.
	if cfg.UserPasswordHash == "" && cfg.PasswordHash != "" {
		cfg.UserPasswordHash, cfg.UserSalt = cfg.PasswordHash, cfg.Salt
	}
	// An existing v1 install has no OP password yet — give it the
	// well-known default so the OP can log in and change it at once.
	// (A fresh v2 install already has one from the branch above.)
	if cfg.AdminPasswordHash == "" {
		if err := setPassword(&cfg, "op", "changeme-op123"); err == nil {
			_ = saveConfig(path, &cfg)
			log.Printf("migrated %s: OP password set to default changeme-op123 — change it with nokhodichat (option 4)", path)
		}
	}
	if cfg.Port == 0 {
		cfg.Port = 44044
	}
	if cfg.Host == "" {
		cfg.Host = "0.0.0.0"
	}
	if cfg.SessionDays <= 0 {
		cfg.SessionDays = 7
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.Title == "" {
		cfg.Title = "اتاق نخودی"
	}
	// Upload limits. v2.2.2 changed them (50 MB/file, 5 files/week,
	// 150 MB/day, 200 MB stored), so a config from v2.2.0/2.2.1 — which
	// has no weekly limit at all — is migrated to the new limits in
	// one go and saved. A config that already has a weekly limit
	// keeps the owner's numbers; only missing (0) values get defaults.
	if cfg.MaxUploadsPerUserPerWeek <= 0 {
		cfg.MaxUploadMB = 50
		cfg.MaxUploadsPerUserPerWeek = 5
		cfg.MaxUploadBytesPerUserPerDay = 150 * 1024 * 1024
		cfg.MaxUploadBytesPerUserTotal = 200 * 1024 * 1024
		_ = saveConfig(path, &cfg)
	} else {
		if cfg.MaxUploadMB <= 0 {
			cfg.MaxUploadMB = 50
		}
		if cfg.MaxUploadBytesPerUserPerDay <= 0 {
			cfg.MaxUploadBytesPerUserPerDay = 150 * 1024 * 1024
		}
		if cfg.MaxUploadBytesPerUserTotal <= 0 {
			cfg.MaxUploadBytesPerUserTotal = 200 * 1024 * 1024
		}
	}
	return &cfg, nil
}

func saveConfig(path string, cfg *Config) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600: credentials live in this file.
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func main() {
	configPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()

	// Small CLI helpers used by manage.sh, e.g.:
	//   ./nokhodi-chat passwd -role op -pass 'new-op-secret'
	//   ./nokhodi-chat passwd -role user -pass 'new-user-secret'
	//   ./nokhodi-chat tls -enable true -mode selfsigned -cert tls/cert.pem -key tls/key.pem
	//   ./nokhodi-chat tls -enable false   (back to plain HTTP, cert files untouched)
	if len(flag.Args()) > 0 {
		switch flag.Arg(0) {
		case "version":
			fmt.Printf("nokhodi-chat %s\n", version)
			return
		case "passwd":
			if err := runPasswd(*configPath); err != nil {
				log.Fatal(err)
			}
			return
		case "tls":
			if err := runTLS(*configPath); err != nil {
				log.Fatal(err)
			}
			return
		case "secret":
			if err := runSecret(*configPath); err != nil {
				log.Fatal(err)
			}
			return
		}
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	srv, err := NewServer(cfg, *configPath)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	if cfg.TLSEnabled {
		if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
			log.Fatalf("TLS is enabled in %s but tls_cert_file / tls_key_file is missing — disable it with: nokhodi-chat tls -enable false", *configPath)
		}
		log.Printf("اتاق نخودی — nokhodi-chat %s listening on https://%s (TLS %s)", version, addr, cfg.TLSMode)
		log.Printf("open https://<server-ip>:%d  (self-signed certs show a browser warning — that is normal)", cfg.Port)
		if err := http.ListenAndServeTLS(addr, cfg.TLSCertFile, cfg.TLSKeyFile, srv.routes()); err != nil {
			log.Fatal(err)
		}
		return
	}
	log.Printf("اتاق نخودی — nokhodi-chat %s listening on http://%s", version, addr)
	log.Printf("open http://<server-ip>:%d  (user changeme123 / OP changeme-op123 by default — change both!)", cfg.Port)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}

// runPasswd implements `./nokhodi-chat passwd -role op|user -pass Y`.
// It is how manage.sh changes the two entry passwords safely:
// it re-hashes with a fresh salt and writes only the hash to config.json.
func runPasswd(configPath string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	role := fs.String("role", "user", "which password to set: op or user")
	pass := fs.String("pass", "", "new password")
	cfgPath := fs.String("config", configPath, "path to config.json")
	_ = fs.Parse(os.Args[2:])

	if *pass == "" {
		return fmt.Errorf("usage: nokhodi-chat passwd -role op|user -pass <password> [-config config.json]")
	}
	if len(*pass) < 6 {
		return fmt.Errorf("password must be at least 6 characters")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if err := setPassword(cfg, *role, *pass); err != nil {
		return err
	}
	if err := saveConfig(*cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("%s password updated in %s (stored as salted hash only)\n", *role, *cfgPath)
	return nil
}

// runTLS implements `./nokhodi-chat tls ...` — how manage.sh turns SSL
// on/off and points the server at a certificate, without hand-editing
// config.json.
//
// Enable (self-signed):
//
//	nokhodi-chat tls -enable true -mode selfsigned -cert tls/cert.pem -key tls/key.pem [-domain example.com]
//
// Enable (Let's Encrypt):
//
//	nokhodi-chat tls -enable true -mode letsencrypt -domain example.com \
//	  -cert /etc/letsencrypt/live/example.com/fullchain.pem \
//	  -key /etc/letsencrypt/live/example.com/privkey.pem
//
// Disable (back to plain HTTP; the cert files are left on disk):
//
//	nokhodi-chat tls -enable false
//
// Clear everything (delete the TLS settings from config.json):
//
//	nokhodi-chat tls -clear
func runTLS(configPath string) error {
	fs := flag.NewFlagSet("tls", flag.ExitOnError)
	// String, not Bool: Go's flag package does not accept `-enable true`
	// (a bool flag takes its value only as `-enable=true`), and every
	// caller — manage.sh, README, and the user — writes `-enable true`.
	enableStr := fs.String("enable", "", "true to enable TLS, false to disable it")
	clear := fs.Bool("clear", false, "remove all TLS settings from config.json")
	mode := fs.String("mode", "", "selfsigned or letsencrypt")
	domain := fs.String("domain", "", "domain name for the certificate (Let's Encrypt, or SAN on a self-signed cert)")
	cert := fs.String("cert", "", "path to the certificate file")
	key := fs.String("key", "", "path to the private key file")
	cfgPath := fs.String("config", configPath, "path to config.json")
	_ = fs.Parse(os.Args[2:])

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *clear {
		cfg.TLSEnabled, cfg.TLSMode, cfg.TLSDomain, cfg.TLSCertFile, cfg.TLSKeyFile = false, "", "", "", ""
		if err := saveConfig(*cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("TLS settings cleared from %s (plain HTTP)\n", *cfgPath)
		return nil
	}
	enable := false
	if *enableStr != "" {
		v, err := strconv.ParseBool(*enableStr)
		if err != nil {
			return fmt.Errorf("usage: nokhodi-chat tls -enable true|false [-mode selfsigned|letsencrypt -cert <file> -key <file>]")
		}
		enable = v
	} else if !*clear {
		return fmt.Errorf("usage: nokhodi-chat tls -enable true|false [-mode selfsigned|letsencrypt -cert <file> -key <file>] or -clear")
	}
	if !enable {
		cfg.TLSEnabled = false
		if err := saveConfig(*cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("TLS disabled in %s (plain HTTP — certificate files were left on disk)\n", *cfgPath)
		return nil
	}
	if *mode != "selfsigned" && *mode != "letsencrypt" {
		return fmt.Errorf("usage: nokhodi-chat tls -enable true -mode selfsigned|letsencrypt -cert <file> -key <file> [-domain <name>]")
	}
	if *cert == "" || *key == "" {
		return fmt.Errorf("a certificate and a key file are required: -cert <file> -key <file>")
	}
	if _, err := os.Stat(*cert); err != nil {
		return fmt.Errorf("certificate file not found: %s", *cert)
	}
	if _, err := os.Stat(*key); err != nil {
		return fmt.Errorf("key file not found: %s", *key)
	}
	cfg.TLSEnabled, cfg.TLSMode, cfg.TLSDomain, cfg.TLSCertFile, cfg.TLSKeyFile = true, *mode, *domain, *cert, *key
	if err := saveConfig(*cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("TLS enabled in %s (mode %s, domain %q) — restart the service to use https\n", *cfgPath, *mode, *domain)
	return nil
}

// runSecret implements `./nokhodi-chat secret ...` — how manage.sh
// and the OP panel turn the secret front door on/off, without
// hand-editing config.json.
//
//	nokhodi-chat secret -enable true    (generates a random path if none yet)
//	nokhodi-chat secret -enable false   (back to plain "/" — front door off)
//	nokhodi-chat secret -regenerate     (new random path, old one stops working)
//	nokhodi-chat secret -show           (prints the current path, or "off")
func runSecret(configPath string) error {
	fs := flag.NewFlagSet("secret", flag.ExitOnError)
	enableStr := fs.String("enable", "", "true to enable the secret front door, false to disable it")
	regenerate := fs.Bool("regenerate", false, "generate a new random secret path")
	show := fs.Bool("show", false, "print the current secret path")
	cfgPath := fs.String("config", configPath, "path to config.json")
	_ = fs.Parse(os.Args[2:])

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *show {
		if cfg.SecretPath == "" {
			fmt.Println("secret front door: off (chat opens at /)")
		} else {
			fmt.Printf("secret front door: on — chat opens at /%s\n", cfg.SecretPath)
		}
		return nil
	}
	if *regenerate {
		code, err := newRandomCode(14)
		if err != nil {
			return err
		}
		cfg.SecretPath = code
		if err := saveConfig(*cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("new secret front door: /%s (old link stops working — restart the service)\n", code)
		return nil
	}
	if *enableStr == "" {
		return fmt.Errorf("usage: nokhodi-chat secret -enable true|false | -regenerate | -show")
	}
	enable, err := strconv.ParseBool(*enableStr)
	if err != nil {
		return fmt.Errorf("usage: nokhodi-chat secret -enable true|false")
	}
	if !enable {
		cfg.SecretPath = ""
		if err := saveConfig(*cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("secret front door disabled in %s (chat opens at / again)\n", *cfgPath)
		return nil
	}
	if cfg.SecretPath == "" {
		code, err := newRandomCode(14)
		if err != nil {
			return err
		}
		cfg.SecretPath = code
	}
	if err := saveConfig(*cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("secret front door enabled in %s — chat opens at /%s (restart the service)\n", *cfgPath, cfg.SecretPath)
	return nil
}
