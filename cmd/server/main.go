package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sahand363/sahand-cortext/internal/channel"
	"github.com/sahand363/sahand-cortext/internal/memory"
	"github.com/sahand363/sahand-cortext/internal/orchestration"
	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
	"github.com/sahand363/sahand-cortext/internal/skill"
	"github.com/sahand363/sahand-cortext/internal/telegram"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const AppVersion = "0.9.7"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(AppVersion)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: sahand-cortext hash-password <password>")
			os.Exit(2)
		}
		h, e := bcrypt.GenerateFromPassword([]byte(os.Args[2]), bcrypt.DefaultCost)
		if e != nil {
			log.Fatal(e)
		}
		fmt.Println(string(h))
		return
	}

	dataDir := env("HUB_DATA_DIR", "./data")
	_ = os.MkdirAll(dataDir, 0700)
	// A staged restore (from a backup import) becomes live on restart.
	restoreDB(dataDir)
	db, e := sql.Open("sqlite", filepath.Join(dataDir, "cortext.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	startWatchdogHeartbeat(db)

	router := provider.NewRouter()
	personaStore := persona.NewStore(db)
	skillStore := skill.NewStore(db)
	memoryStore := memory.NewStore(db)
	orchStorage := orchestration.NewStorage(db)

	app := &App{
		db:             db,
		sessions:       map[string]time.Time{},
		webStagedSkill: map[string]*skill.StagedDraft{},
		router:         router,
		personaStore:   personaStore,
		skillStore:     skillStore,
		memoryStore:    memoryStore,
		orchStorage:    orchStorage,
		loginLimit:     newLoginLimiter(),
	}

	tgBridge := telegram.NewBridgeManager(db, personaStore, skillStore, memoryStore, router, app.providerKey)
	if err := tgBridge.EnablePrivateChats(dataDir); err != nil {
		log.Printf("[Telegram] Encrypted chat storage unavailable, bots will refuse to chat: %v", err)
	}
	bridgeCtx, bridgeCancel := context.WithCancel(context.Background())
	tgBridge.StartSync(bridgeCtx)
	app.startMemoryExtractor()
	app.channels = channel.NewService(channel.NewStore(db), personaStore, router, app.providerKey, tgBridge.ResolveProviderAndModel)

	collabEngine := orchestration.NewCollaborationEngine(
		orchStorage,
		router,
		app.providerKey,
		func() (string, string) { return app.activeProvider(), app.activeModel() },
		orchestration.DefaultCollaborationConfig(),
	)
	collabEngine.SetProviderConfiguredChecker(app.isProviderConfigured)
	collabEngine.SetFallbackCandidatesProvider(app.verifiedWorkingParticipants)

	orchPool := orchestration.NewWorkerPool(orchStorage, collabEngine, envInt("ORCHESTRATION_WORKERS", 2))
	app.orchEngine = collabEngine
	app.orchPool = orchPool

	if e = app.initDB(); e != nil {
		log.Fatal(e)
	}

	app.channels.Start(bridgeCtx)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	orchPool.Start(workerCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", app.login)
	mux.HandleFunc("/api/logout", app.auth(app.logout))
	mux.HandleFunc("/api/status", app.auth(app.status))
	mux.HandleFunc("/api/providers", app.auth(app.handleProviders))
	mux.HandleFunc("/api/providers/", app.auth(app.handleProviderByID))
	mux.HandleFunc("/api/personas", app.auth(app.handlePersonas))
	mux.HandleFunc("/api/personas/train", app.auth(app.handleTrainPersona))
	mux.HandleFunc("/api/personas/", app.auth(app.handlePersonaByID))
	mux.HandleFunc("/api/channels", app.auth(app.handleChannels))
	mux.HandleFunc("/api/channels/", app.auth(app.handleChannelByID))
	mux.HandleFunc("/api/channel-posts/", app.auth(app.handleChannelPost))
	mux.HandleFunc("/api/skills", app.auth(app.handleSkills))
	mux.HandleFunc("/api/skills/", app.auth(app.handleSkillByID))
	mux.HandleFunc("/api/memories", app.auth(app.handleMemories))
	mux.HandleFunc("/api/memories/extract", app.auth(app.handleMemoryExtract))
	mux.HandleFunc("/api/memories/", app.auth(app.handleMemoryByID))
	mux.HandleFunc("/api/setup/gemini", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/setup/provider", app.auth(app.saveProviderKey))
	mux.HandleFunc("/api/models", app.auth(app.handleModels))
	mux.HandleFunc("/api/models/test", app.auth(app.handleTestModel))
	mux.HandleFunc("/api/models/toggle", app.auth(app.handleToggleModel))
	mux.HandleFunc("/api/models/custom", app.auth(app.handleAddCustomModel))
	mux.HandleFunc("/api/chat/model-scope", app.auth(app.handleChatModelScope))
	mux.HandleFunc("/api/usage/summary", app.auth(app.usageSummary))

	// Backup (Export / Import): take everything with you — one zip with
	// the database plus the env secrets a fresh hub needs to restore it.
	mux.HandleFunc("/api/backup/export", app.auth(app.backupExport))
	mux.HandleFunc("/api/backup/import", app.auth(app.backupImport))

	// Mobile (hub-owned): the Android app talks to CorText directly or
	// through a dumb relay. The encrypted endpoints verify the mobile token
	// by decrypting the body; the status/settings endpoints use the web login.
	mux.HandleFunc("/api/mobile/health", app.mobileHealth)
	mux.HandleFunc("/api/mobile/sessions", app.mobileSessions)
	mux.HandleFunc("/api/mobile/sessions/list", app.mobileSessions)
	mux.HandleFunc("/api/mobile/sessions/", app.mobileSessionByID)
	mux.HandleFunc("/api/mobile/personas", app.mobilePersonas)
	mux.HandleFunc("/api/mobile/chat", app.mobileChat)
	mux.HandleFunc("/api/mobile/chat/stream", app.mobileChatStream)
	mux.HandleFunc("/api/mobile/models", app.mobileModels)
	mux.HandleFunc("/api/mobile/history", app.mobileHistory)
	mux.HandleFunc("/api/mobile/status", app.auth(app.mobileStatus))
	mux.HandleFunc("/api/mobile/settings", app.auth(app.mobileSettings))

	mux.HandleFunc("/api/sessions", app.auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			app.listSessions(w, r)
		case http.MethodPost:
			app.createSession(w, r)
		case http.MethodPatch, http.MethodPut:
			app.updateSession(w, r)
		case http.MethodDelete:
			app.deleteSession(w, r)
		default:
			http.Error(w, "method not allowed", 405)
		}
	}))
	mux.HandleFunc("/api/messages", app.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			app.getMessages(w, r)
		} else if r.Method == http.MethodPost {
			app.sendMessage(w, r)
		} else {
			http.Error(w, "method not allowed", 405)
		}
	}))

	mux.HandleFunc("/api/orchestration/jobs", app.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			app.orchListJobs(w, r)
		} else if r.Method == http.MethodPost {
			app.orchCreateJob(w, r)
		} else {
			http.Error(w, "method not allowed", 405)
		}
	}))
	mux.HandleFunc("/api/orchestration/jobs/", app.auth(app.orchGetJob))

	webDir := env("HUB_WEB_DIR", "./web/dist")
	// Serve the panel with Cache-Control: no-cache, so a browser never
	// shows a stale panel after an update (the files change on every
	// release, and a normal refresh must always fetch the current
	// index.html/app.js — no hard refresh needed).
	webFiles := http.FileServer(http.Dir(webDir))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		webFiles.ServeHTTP(w, r)
	}))

	server := &http.Server{
		Addr:              env("HUB_ADDR", "0.0.0.0:8787"),
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("[Server] Graceful shutdown signal received. Stopping services...")
		bridgeCancel()
		workerCancel()
		orchPool.Stop()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if !provider.APIKeyEncryptionReady() {
		log.Printf("[Provider] WARNING: HUB_KEY_ENCRYPTION_KEY is not set; provider API keys are stored unencrypted. Run the installer or 'sudo cortext update' to generate one.")
	}
	log.Printf("Sahand-CorText v%s listening on %s", AppVersion, server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
