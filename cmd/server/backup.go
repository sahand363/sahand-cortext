package main

// Backup (Export / Import) — take everything with you.
//
// The hub stores everything in one SQLite database (cortext.db in
// HUB_DATA_DIR): chats, personas, skills, memories, settings and the
// mobile token. The provider API keys in that database are encrypted with
// a key that lives in the env file (/etc/sahand-cortext.env), NOT in the
// database — so a backup of the database alone is not enough. A backup
// therefore bundles TWO things: a consistent snapshot of the database and
// the three env secrets a fresh hub needs to restore it (HUB_USERNAME,
// HUB_PASSWORD_HASH, HUB_KEY_ENCRYPTION_KEY).
//
// Export (GET /api/backup/export, web login): builds one .zip with
// cortext.db + env.txt + manifest.json and returns it as a download.
// The database snapshot is made with VACUUM INTO, which produces a
// consistent copy of a live WAL-mode database — a raw copy of the live
// cortext.db file can be corrupt/inconsistent, so it is never copied
// directly.
//
// Import (POST /api/backup/import, web login, multipart file): validates
// the zip (manifest app, SQLite header, and that the database opens and
// has the settings table), then stages the database as
// cortext.db.restored. The staged database becomes live on the next
// restart (main() renames it into place before opening the DB — see
// restoreDB). The env secrets are written to /etc/sahand-cortext.env when
// the hub is allowed to; otherwise they are staged as restore.env and
// the panel shows the exact one-liner to apply them. Nothing is changed
// unless the whole backup validates first, and the import never logs the
// user out mid-request — the current session stays valid until the
// restart, after which the restored password applies.

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// backupEnvPath is where the installer puts the hub's env file. The
	// hub runs as user cortext and this file is root-owned 0600, so the
	// hub usually CANNOT write it — the import falls back to staging
	// restore.env and showing a one-liner in that case.
	backupEnvPath = "/etc/sahand-cortext.env"

	// backupMaxImportBytes caps an uploaded backup (1 GiB). A backup is
	// normally far smaller; this only stops an absurd upload.
	backupMaxImportBytes = 1 << 30
)

// backupEnvKeys are the env secrets a backup must carry. They are read
// from the process environment (the hub already has them); the hub never
// reads the env file from disk.
var backupEnvKeys = []string{"HUB_USERNAME", "HUB_PASSWORD_HASH", "HUB_KEY_ENCRYPTION_KEY"}

// backupDataDir returns the hub's data directory (where cortext.db lives).
func backupDataDir() string { return env("HUB_DATA_DIR", "./data") }

// backupEnvText renders the restore secrets as KEY=VALUE lines. Only
// non-empty values are included.
func backupEnvText() string {
	var b strings.Builder
	for _, k := range backupEnvKeys {
		if v := os.Getenv(k); v != "" {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	return b.String()
}

// restoreDB applies a staged backup database, if one exists: it replaces
// the live cortext.db (and drops the WAL sidecars, which belong to the
// old database). Called at startup, before the DB is opened, so a backup
// import takes effect on the next restart.
func restoreDB(dataDir string) {
	staged := filepath.Join(dataDir, "cortext.db.restored")
	if _, err := os.Stat(staged); err != nil {
		return
	}
	live := filepath.Join(dataDir, "cortext.db")
	_ = os.Remove(live)
	_ = os.Remove(live + "-wal")
	_ = os.Remove(live + "-shm")
	if err := os.Rename(staged, live); err != nil {
		log.Printf("[Backup] WARNING: could not apply staged restore %s: %v", staged, err)
		return
	}
	log.Printf("[Backup] Applied staged restore: %s -> %s", staged, live)
}

// addFileToZip copies a file from disk into the zip under name.
func addFileToZip(zw *zip.Writer, diskPath, name string) error {
	f, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// backupExport builds one backup .zip (cortext.db + env.txt +
// manifest.json) and returns it as a download.
func (a *App) backupExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	// 1. A consistent snapshot of the live database. VACUUM INTO writes
	// a clean standalone copy; the live file is never copied directly.
	tmpDir, err := os.MkdirTemp("", "cortext-backup-")
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "cannot create temp dir"})
		return
	}
	defer os.RemoveAll(tmpDir)
	snapPath := filepath.Join(tmpDir, "cortext.db")
	safe := strings.ReplaceAll(snapPath, "'", "''")
	if _, err := a.db.Exec("VACUUM INTO '" + safe + "'"); err != nil {
		jsonOut(w, 500, map[string]string{"error": "database snapshot failed: " + err.Error()})
		return
	}
	// 2. Build the zip into a temp file first, so a failure still returns
	// a clean JSON error instead of a half-written download.
	zipPath := filepath.Join(tmpDir, "backup.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "cannot create backup file"})
		return
	}
	zw := zip.NewWriter(zf)
	if err := addFileToZip(zw, snapPath, "cortext.db"); err != nil {
		_ = zw.Close()
		_ = zf.Close()
		jsonOut(w, 500, map[string]string{"error": "cannot add database to backup: " + err.Error()})
		return
	}
	ew, err := zw.Create("env.txt")
	if err == nil {
		_, err = io.WriteString(ew, backupEnvText())
	}
	if err != nil {
		_ = zw.Close()
		_ = zf.Close()
		jsonOut(w, 500, map[string]string{"error": "cannot add env.txt to backup: " + err.Error()})
		return
	}
	manifest := map[string]any{
		"app":        "sahand-cortext",
		"version":    AppVersion,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"contains":   []string{"cortext.db", "env.txt"},
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	mw, err := zw.Create("manifest.json")
	if err == nil {
		_, err = mw.Write(mb)
	}
	if err != nil {
		_ = zw.Close()
		_ = zf.Close()
		jsonOut(w, 500, map[string]string{"error": "cannot add manifest to backup: " + err.Error()})
		return
	}
	if err := zw.Close(); err != nil {
		_ = zf.Close()
		jsonOut(w, 500, map[string]string{"error": "cannot finish backup: " + err.Error()})
		return
	}
	_ = zf.Close()
	// Open the finished zip BEFORE setting any headers, so a failure
	// here still returns a clean JSON error (a Content-Length set
	// earlier would not match the short error body).
	rf, err := os.Open(zipPath)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "cannot open backup file"})
		return
	}
	defer rf.Close()
	st, err := rf.Stat()
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "cannot stat backup file"})
		return
	}
	// 3. Serve it as a download.
	fname := "cortext-backup-" + time.Now().Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	_, _ = io.Copy(w, rf)
	log.Printf("[Backup] Exported %s (%d bytes)", fname, st.Size())
}

// validateBackupDB checks that path is a readable CorText database: it
// must open and contain the settings table.
func validateBackupDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var name string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='settings'").Scan(&name); err != nil {
		return fmt.Errorf("settings table not found (not a CorText database)")
	}
	return nil
}

// applyEnvFile merges the given KEY=VALUE lines into the env file at
// path: the backup keys are replaced (or appended if absent); every other
// line is kept as-is.
func applyEnvFile(path, envText string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	want := map[string]string{}
	for _, line := range strings.Split(envText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			want[line[:i]] = line[i+1:]
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key := trimmed
		if i := strings.Index(trimmed, "="); i > 0 {
			key = trimmed[:i]
		}
		if v, ok := want[key]; ok {
			out = append(out, key+"="+v)
			seen[key] = true
		} else {
			out = append(out, line)
		}
	}
	for _, k := range backupEnvKeys {
		if v, ok := want[k]; ok && !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0600)
}

// backupEnvOneliner builds the one-liner the user runs (with sudo) when
// the hub could not write the env file itself: it merges restore.env
// into /etc/sahand-cortext.env and restarts the hub.
func backupEnvOneliner(restoreEnvPath string) string {
	return "sudo bash -c 'f=" + backupEnvPath + "; touch $f; grep -v -e ^HUB_USERNAME= -e ^HUB_PASSWORD_HASH= -e ^HUB_KEY_ENCRYPTION_KEY= $f > /tmp/cortext.env.new; cat " + restoreEnvPath + " >> /tmp/cortext.env.new; mv /tmp/cortext.env.new $f; chmod 600 $f; systemctl restart sahand-cortext'"
}

// backupImport validates an uploaded backup zip and stages the restore.
// The database becomes live on the next restart (see restoreDB). The
// response always completes before any restart, so the user is not
// logged out mid-request.
func (a *App) backupImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backupMaxImportBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonOut(w, 400, map[string]string{"error": "cannot read upload (too large or not a form)"})
		return
	}
	fh, _, err := r.FormFile("file")
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "no file uploaded"})
		return
	}
	defer fh.Close()
	tmpDir, err := os.MkdirTemp("", "cortext-restore-")
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "cannot create temp dir"})
		return
	}
	defer os.RemoveAll(tmpDir)
	upPath := filepath.Join(tmpDir, "upload.zip")
	up, err := os.Create(upPath)
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": "cannot save upload"})
		return
	}
	if _, err := io.Copy(up, fh); err != nil {
		_ = up.Close()
		jsonOut(w, 400, map[string]string{"error": "cannot read upload"})
		return
	}
	_ = up.Close()

	// Validate the zip: it must hold cortext.db + manifest.json.
	zr, err := zip.OpenReader(upPath)
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "not a valid zip file"})
		return
	}
	defer zr.Close()
	var dbEntry, manifestEntry, envEntry *zip.File
	for _, f := range zr.File {
		switch f.Name {
		case "cortext.db":
			dbEntry = f
		case "manifest.json":
			manifestEntry = f
		case "env.txt":
			envEntry = f
		}
	}
	if dbEntry == nil || manifestEntry == nil {
		jsonOut(w, 400, map[string]string{"error": "not a CorText backup: missing cortext.db or manifest.json"})
		return
	}
	var manifest struct {
		App     string `json:"app"`
		Version string `json:"version"`
	}
	mrc, err := manifestEntry.Open()
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "cannot read manifest.json"})
		return
	}
	merr := json.NewDecoder(mrc).Decode(&manifest)
	_ = mrc.Close()
	if merr != nil {
		jsonOut(w, 400, map[string]string{"error": "manifest.json is not valid JSON"})
		return
	}
	if manifest.App != "sahand-cortext" {
		jsonOut(w, 400, map[string]string{"error": "not a CorText backup (manifest app is not sahand-cortext)"})
		return
	}
	// The database must start with the SQLite header.
	drc, err := dbEntry.Open()
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "cannot read cortext.db from the backup"})
		return
	}
	head := make([]byte, 16)
	_, herr := io.ReadFull(drc, head)
	_ = drc.Close()
	if herr != nil || string(head) != "SQLite format 3\x00" {
		jsonOut(w, 400, map[string]string{"error": "cortext.db is not a valid SQLite file"})
		return
	}
	// Stage the database and check it really is a CorText database.
	// Nothing is changed unless the whole backup validates first.
	dataDir := backupDataDir()
	stagedDB := filepath.Join(dataDir, "cortext.db.restored")
	drc2, err := dbEntry.Open()
	if err != nil {
		jsonOut(w, 400, map[string]string{"error": "cannot read cortext.db from the backup"})
		return
	}
	sf, err := os.OpenFile(stagedDB, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = drc2.Close()
		jsonOut(w, 500, map[string]string{"error": "cannot stage the restored database: " + err.Error()})
		return
	}
	_, cerr := io.Copy(sf, drc2)
	_ = drc2.Close()
	_ = sf.Close()
	if cerr != nil {
		_ = os.Remove(stagedDB)
		jsonOut(w, 500, map[string]string{"error": "cannot stage the restored database: " + cerr.Error()})
		return
	}
	if err := validateBackupDB(stagedDB); err != nil {
		_ = os.Remove(stagedDB)
		jsonOut(w, 400, map[string]string{"error": "backup database is not a valid CorText database: " + err.Error()})
		return
	}
	// Env secrets: apply them directly if the hub is allowed to,
	// otherwise stage them and hand the user a one-liner.
	envApplied := false
	envNote := ""
	envOneliner := ""
	if envEntry != nil {
		erc, err := envEntry.Open()
		if err != nil {
			_ = os.Remove(stagedDB)
			jsonOut(w, 400, map[string]string{"error": "cannot read env.txt from the backup"})
			return
		}
		envBytes, rerr := io.ReadAll(erc)
		_ = erc.Close()
		if rerr != nil {
			_ = os.Remove(stagedDB)
			jsonOut(w, 400, map[string]string{"error": "cannot read env.txt from the backup"})
			return
		}
		envText := string(envBytes)
		if err := applyEnvFile(backupEnvPath, envText); err == nil {
			envApplied = true
		} else {
			restoreEnv := filepath.Join(dataDir, "restore.env")
			if werr := os.WriteFile(restoreEnv, []byte(envText), 0600); werr != nil {
				_ = os.Remove(stagedDB)
				jsonOut(w, 500, map[string]string{"error": "cannot stage the restore secrets: " + werr.Error()})
				return
			}
			absDir, _ := filepath.Abs(dataDir)
			envNote = "Could not write " + backupEnvPath + " (permission). The secrets were saved to restore.env — apply them with the command below, which also restarts the hub."
			envOneliner = backupEnvOneliner(filepath.Join(absDir, "restore.env"))
		}
	} else {
		envNote = "This backup has no env.txt, so the login password and API-key encryption key were NOT restored. After the restart, set them again (or re-enter the provider API keys)."
	}
	jsonOut(w, 200, map[string]any{
		"ok":             true,
		"backup_version": manifest.Version,
		"restored_db":    "cortext.db.restored — becomes live on restart",
		"env_applied":    envApplied,
		"env_note":       envNote,
		"env_oneliner":   envOneliner,
		"restart":        "sudo systemctl restart sahand-cortext",
		"note":           "Restart the hub to finish the restore. After the restart you will be logged out — sign in with the restored password.",
	})
	log.Printf("[Backup] Import staged: db=%s env_applied=%v", stagedDB, envApplied)
}
