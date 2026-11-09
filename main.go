package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jchv/go-webview2"
)

// ModelInfo holds metadata from Google's /v1beta/models endpoint
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// TierModels holds the automatically resolved model IDs for each operating mode
type TierModels struct {
	Eco       string `json:"eco"`
	Workhorse string `json:"workhorse"`
	Deep      string `json:"deep"`
}

// AppConfig stores persistent local settings and tier preferences
type AppConfig struct {
	GeminiAPIKey    string      `json:"gemini_api_key"`
	StartupTier     string      `json:"startup_tier"` // "eco", "workhorse", "deep", "custom"
	ActiveTier      string      `json:"active_tier"`
	CustomModel     string      `json:"custom_model"`
	TierMap         TierModels  `json:"tier_map"`
	AvailableModels []ModelInfo `json:"available_models"`
	SlidingWindow   int         `json:"sliding_window"`
	Verified        bool        `json:"verified"`
	LastTestedAt    string      `json:"last_tested_at"`
}

// ChatMessage represents a single turn in a conversation
type ChatMessage struct {
	ID               int64  `json:"id"`
	SessionID        int64  `json:"session_id"`
	Role             string `json:"role"` // "user" or "model"
	Content          string `json:"content"`
	ModelUsed        string `json:"model_used"`
	PromptTokens     int    `json:"prompt_tokens"`
	CandidateTokens  int    `json:"candidate_tokens"`
	ThoughtTokens    int    `json:"thought_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	LatencyMs        int64  `json:"latency_ms"`
	CreatedAt        string `json:"created_at"`
}

// ChatSession represents a conversation thread
type ChatSession struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	TotalTokens int    `json:"total_tokens"`
	UpdatedAt   string `json:"updated_at"`
}

// DailyStats tracks daily token and request consumption
type DailyStats struct {
	Date            string `json:"date"` // YYYY-MM-DD
	RequestsCount   int    `json:"requests_count"`
	PromptTokens    int    `json:"prompt_tokens"`
	CandidateTokens int    `json:"candidate_tokens"`
	ThoughtTokens   int    `json:"thought_tokens"`
	TotalTokens     int    `json:"total_tokens"`
}

// LocalDatabase holds all sessions, messages, and usage telemetry on disk
type LocalDatabase struct {
	NextSessionID int64                  `json:"next_session_id"`
	NextMessageID int64                  `json:"next_message_id"`
	Sessions      []ChatSession          `json:"sessions"`
	Messages      map[int64][]ChatMessage `json:"messages"`
	DailyUsage    map[string]DailyStats  `json:"daily_usage"`
}

var dbMutex sync.Mutex

func getAppDir() string {
	appData, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(appData, "SahandCorText")
	_ = os.MkdirAll(dir, 0700)
	return dir
}

func getConfigPath() string {
	return filepath.Join(getAppDir(), "config.json")
}

func getDBPath() string {
	return filepath.Join(getAppDir(), "cortext_db.json")
}

func defaultModels() []ModelInfo {
	return []ModelInfo{
		{ID: "gemini-3.8-flash-lite", DisplayName: "Gemini 3.8 Flash Lite (Eco / Cheapest)"},
		{ID: "gemini-3.8-flash", DisplayName: "Gemini 3.8 Flash (Workhorse / Default)"},
		{ID: "gemini-4-argon", DisplayName: "Gemini 4 Argon (Deep Thinker / Max Reasoning)"},
		{ID: "gemini-3.5-flash-lite", DisplayName: "Gemini 3.5 Flash Lite"},
		{ID: "gemini-3.5-flash", DisplayName: "Gemini 3.5 Flash"},
	}
}

func resolveTierMap(models []ModelInfo) TierModels {
	tiers := TierModels{
		Eco:       "gemini-3.8-flash-lite",
		Workhorse: "gemini-3.8-flash",
		Deep:      "gemini-4-argon",
	}

	var foundEco, foundWork, foundDeep bool
	for _, m := range models {
		id := strings.ToLower(m.ID)
		if !foundEco && strings.Contains(id, "lite") {
			tiers.Eco = m.ID
			foundEco = true
		}
		if !foundWork && (id == "gemini-3.8-flash" || id == "gemini-3.5-flash" || (strings.Contains(id, "flash") && !strings.Contains(id, "lite"))) {
			tiers.Workhorse = m.ID
			foundWork = true
		}
		if !foundDeep && (strings.Contains(id, "argon") || strings.Contains(id, "pro")) {
			tiers.Deep = m.ID
			foundDeep = true
		}
	}
	return tiers
}

func loadConfig() AppConfig {
	models := defaultModels()
	cfg := AppConfig{
		StartupTier:     "workhorse",
		ActiveTier:      "workhorse",
		CustomModel:     "gemini-3.8-flash",
		TierMap:         resolveTierMap(models),
		AvailableModels: models,
		SlidingWindow:   10,
	}
	data, err := os.ReadFile(getConfigPath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if cfg.StartupTier == "" {
		cfg.StartupTier = "workhorse"
	}
	if cfg.ActiveTier == "" {
		cfg.ActiveTier = cfg.StartupTier
	}
	if cfg.SlidingWindow <= 0 {
		cfg.SlidingWindow = 10
	}
	if len(cfg.AvailableModels) == 0 {
		cfg.AvailableModels = defaultModels()
	}
	// Clean out any cached -tts or retired 2.5 models from previous builds
	var cleaned []ModelInfo
	for _, m := range cfg.AvailableModels {
		if !isExcludedModel(m.ID) {
			cleaned = append(cleaned, m)
		}
	}
	if len(cleaned) == 0 {
		cleaned = defaultModels()
	}
	cfg.AvailableModels = cleaned
	cfg.TierMap = resolveTierMap(cfg.AvailableModels)

	if cfg.CustomModel == "" || isExcludedModel(cfg.CustomModel) {
		cfg.CustomModel = cfg.TierMap.Workhorse
	}
	return cfg
}

func saveConfig(cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0600)
}

func getActiveModel(cfg AppConfig) string {
	switch cfg.ActiveTier {
	case "eco":
		return cfg.TierMap.Eco
	case "deep":
		return cfg.TierMap.Deep
	case "custom":
		if cfg.CustomModel != "" {
			return cfg.CustomModel
		}
		return cfg.TierMap.Workhorse
	default:
		return cfg.TierMap.Workhorse
	}
}

func loadDB() LocalDatabase {
	db := LocalDatabase{
		NextSessionID: 1,
		NextMessageID: 1,
		Sessions:      []ChatSession{},
		Messages:      make(map[int64][]ChatMessage),
		DailyUsage:    make(map[string]DailyStats),
	}
	data, err := os.ReadFile(getDBPath())
	if err == nil {
		_ = json.Unmarshal(data, &db)
	}
	if db.Messages == nil {
		db.Messages = make(map[int64][]ChatMessage)
	}
	if db.DailyUsage == nil {
		db.DailyUsage = make(map[string]DailyStats)
	}
	if len(db.Sessions) == 0 {
		now := time.Now().Format("2006-01-02 15:04")
		sess := ChatSession{
			ID:        1,
			Title:     "General Assistant",
			UpdatedAt: now,
		}
		db.Sessions = append(db.Sessions, sess)
		db.NextSessionID = 2
		_ = saveDB(db)
	}
	return db
}

func saveDB(db LocalDatabase) error {
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getDBPath(), data, 0600)
}

// isExcludedModel filters out TTS, audio, vision-only, and retired models
func isExcludedModel(id string) bool {
	lower := strings.ToLower(id)
	excludedKeywords := []string{
		"tts", "audio", "embedding", "imagen", "veo", "aqa",
		"bisheng", "learnlm", "robotics", "computer-use",
		"1.5", "2.0", "2.5", "live", "thinking-exp",
	}
	for _, kw := range excludedKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func scoreModel(id string) int {
	lower := strings.ToLower(id)
	switch {
	case lower == "gemini-3.8-flash":
		return 1000
	case lower == "gemini-3.8-flash-lite":
		return 980
	case lower == "gemini-4-argon":
		return 960
	case strings.HasPrefix(lower, "gemini-3.8-flash"):
		return 920
	case strings.HasPrefix(lower, "gemini-4"):
		return 900
	case strings.HasPrefix(lower, "gemini-3.8"):
		return 850
	case strings.HasPrefix(lower, "gemini-3.5-flash"):
		return 800
	case strings.HasPrefix(lower, "gemini-3"):
		return 700
	default:
		return 100
	}
}

func fetchAvailableGeminiModels(apiKey string) ([]ModelInfo, TierModels, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, TierModels{}, fmt.Errorf("API key is required")
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s&pageSize=100", apiKey)
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, TierModels{}, fmt.Errorf("Network error: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, TierModels{}, fmt.Errorf("Google API returned HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, TierModels{}, err
	}

	var list []ModelInfo
	for _, m := range parsed.Models {
		supportsGen := false
		for _, method := range m.SupportedGenerationMethods {
			if method == "generateContent" {
				supportsGen = true
				break
			}
		}
		if !supportsGen {
			continue
		}

		id := strings.TrimPrefix(m.Name, "models/")
		if isExcludedModel(id) {
			continue
		}

		display := m.DisplayName
		if display == "" {
			display = id
		} else {
			display = fmt.Sprintf("%s (%s)", m.DisplayName, id)
		}

		list = append(list, ModelInfo{
			ID:          id,
			DisplayName: display,
		})
	}

	if len(list) == 0 {
		list = defaultModels()
	}

	sort.Slice(list, func(i, j int) bool {
		si, sj := scoreModel(list[i].ID), scoreModel(list[j].ID)
		if si != sj {
			return si > sj
		}
		return list[i].ID < list[j].ID
	})

	tiers := resolveTierMap(list)
	return list, tiers, nil
}

// callGeminiGenerate invokes the Gemini REST API with conversation contents
func callGeminiGenerate(apiKey, model string, contents []map[string]interface{}, systemPrompt string) (string, int, int, int, int, int64, error) {
	start := time.Now()
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)

	payload := map[string]interface{}{
		"contents": contents,
	}
	if systemPrompt != "" {
		payload["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]string{
				{"text": systemPrompt},
			},
		}
	}

	bodyBytes, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 60 * time.Second}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return "", 0, 0, 0, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return "", 0, 0, 0, 0, latency, fmt.Errorf("Network error: %v", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errObj map[string]interface{}
		_ = json.Unmarshal(respData, &errObj)
		errMsg := fmt.Sprintf("HTTP %d error from Gemini API", resp.StatusCode)
		if e, ok := errObj["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				errMsg = m
			}
		}
		return "", 0, 0, 0, 0, latency, fmt.Errorf(errMsg)
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}

	if err := json.Unmarshal(respData, &parsed); err != nil {
		return "", 0, 0, 0, 0, latency, err
	}

	var replyBuilder strings.Builder
	if len(parsed.Candidates) > 0 {
		for _, p := range parsed.Candidates[0].Content.Parts {
			if !p.Thought && p.Text != "" {
				replyBuilder.WriteString(p.Text)
			}
		}
	}
	reply := strings.TrimSpace(replyBuilder.String())
	if reply == "" {
		reply = "(No text returned by model)"
	}

	u := parsed.UsageMetadata
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount + u.ThoughtsTokenCount
	}

	return reply, u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount, total, latency, nil
}

func recordDailyUsage(db *LocalDatabase, promptTok, candTok, thoughtTok, totalTok int) DailyStats {
	today := time.Now().Format("2006-01-02")
	stats := db.DailyUsage[today]
	stats.Date = today
	stats.RequestsCount++
	stats.PromptTokens += promptTok
	stats.CandidateTokens += candTok
	stats.ThoughtTokens += thoughtTok
	stats.TotalTokens += totalTok
	db.DailyUsage[today] = stats
	return stats
}

func getTodayStats(db *LocalDatabase) DailyStats {
	today := time.Now().Format("2006-01-02")
	if st, ok := db.DailyUsage[today]; ok {
		return st
	}
	return DailyStats{Date: today}
}

func startLocalServer() (string, error) {
	// On startup, apply the user's preferred StartupTier so Argon is never selected accidentally
	initCfg := loadConfig()
	if initCfg.StartupTier != "" {
		initCfg.ActiveTier = initCfg.StartupTier
		_ = saveConfig(initCfg)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(uiHTML))
	})

	// Initial state bundle for UI startup
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		dbMutex.Lock()
		defer dbMutex.Unlock()

		cfg := loadConfig()
		db := loadDB()
		todayStats := getTodayStats(&db)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"config":       cfg,
			"active_model": getActiveModel(cfg),
			"sessions":     db.Sessions,
			"today_stats":  todayStats,
		})
	})

	// Switch active tier or startup preferences
	mux.HandleFunc("/api/set-tier", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ActiveTier    string `json:"active_tier"`
			StartupTier   string `json:"startup_tier"`
			CustomModel   string `json:"custom_model"`
			SlidingWindow int    `json:"sliding_window"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		cfg := loadConfig()
		if req.ActiveTier != "" {
			cfg.ActiveTier = req.ActiveTier
		}
		if req.StartupTier != "" {
			cfg.StartupTier = req.StartupTier
		}
		if req.CustomModel != "" {
			cfg.CustomModel = req.CustomModel
		}
		if req.SlidingWindow > 0 {
			cfg.SlidingWindow = req.SlidingWindow
		}
		_ = saveConfig(cfg)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":           true,
			"config":       cfg,
			"active_model": getActiveModel(cfg),
		})
	})

	// Discover models & verify API key
	mux.HandleFunc("/api/verify-gemini", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			APIKey      string `json:"api_key"`
			StartupTier string `json:"startup_tier"`
			CustomModel string `json:"custom_model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"valid":   false,
				"message": "Please enter a Gemini API key first.",
			})
			return
		}

		models, tiers, _ := fetchAvailableGeminiModels(apiKey)
		if len(models) == 0 {
			models = defaultModels()
			tiers = resolveTierMap(models)
		}

		cfg := loadConfig()
		cfg.GeminiAPIKey = apiKey
		cfg.AvailableModels = models
		cfg.TierMap = tiers
		if req.StartupTier != "" {
			cfg.StartupTier = req.StartupTier
			cfg.ActiveTier = req.StartupTier
		}
		if req.CustomModel != "" && !isExcludedModel(req.CustomModel) {
			cfg.CustomModel = req.CustomModel
		}

		testModel := getActiveModel(cfg)
		contents := []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]string{
					{"text": "Reply with the single word: ONLINE"},
				},
			},
		}

		reply, pTok, cTok, tTok, totTok, latency, err := callGeminiGenerate(apiKey, testModel, contents, "")
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"valid":            false,
				"message":          err.Error(),
				"latency_ms":       latency,
				"available_models": models,
				"tier_map":         tiers,
			})
			return
		}

		cfg.Verified = true
		cfg.LastTestedAt = time.Now().Format("2006-01-02 15:04:05")
		_ = saveConfig(cfg)

		dbMutex.Lock()
		db := loadDB()
		todayStats := recordDailyUsage(&db, pTok, cTok, tTok, totTok)
		_ = saveDB(db)
		dbMutex.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":        true,
			"message":      fmt.Sprintf("Verified! Bound to %s (%d tokens)", testModel, totTok),
			"model_used":   testModel,
			"latency_ms":   latency,
			"reply":        reply,
			"config":       cfg,
			"today_stats":  todayStats,
		})
	})

	// Session management: Get messages for a session
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		dbMutex.Lock()
		defer dbMutex.Unlock()

		var sessionID int64
		_, _ = fmt.Sscanf(r.URL.Query().Get("session_id"), "%d", &sessionID)

		db := loadDB()
		msgs := db.Messages[sessionID]
		if msgs == nil {
			msgs = []ChatMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"messages": msgs,
		})
	})

	// Create new chat session
	mux.HandleFunc("/api/session/new", func(w http.ResponseWriter, r *http.Request) {
		dbMutex.Lock()
		defer dbMutex.Unlock()

		db := loadDB()
		newSess := ChatSession{
			ID:        db.NextSessionID,
			Title:     "New Chat",
			UpdatedAt: time.Now().Format("2006-01-02 15:04"),
		}
		db.NextSessionID++
		db.Sessions = append([]ChatSession{newSess}, db.Sessions...)
		_ = saveDB(db)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"session":  newSess,
			"sessions": db.Sessions,
		})
	})

	// Delete a chat session
	mux.HandleFunc("/api/session/delete", func(w http.ResponseWriter, r *http.Request) {
		dbMutex.Lock()
		defer dbMutex.Unlock()

		var req struct {
			SessionID int64 `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		db := loadDB()
		delete(db.Messages, req.SessionID)
		var remaining []ChatSession
		for _, s := range db.Sessions {
			if s.ID != req.SessionID {
				remaining = append(remaining, s)
			}
		}
		if len(remaining) == 0 {
			remaining = append(remaining, ChatSession{
				ID:        db.NextSessionID,
				Title:     "General Assistant",
				UpdatedAt: time.Now().Format("2006-01-02 15:04"),
			})
			db.NextSessionID++
		}
		db.Sessions = remaining
		_ = saveDB(db)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sessions": db.Sessions,
		})
	})

	// Send chat message with sliding window context
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID int64  `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		userText := strings.TrimSpace(req.Text)
		if userText == "" {
			http.Error(w, "Empty message", http.StatusBadRequest)
			return
		}

		cfg := loadConfig()
		if cfg.GeminiAPIKey == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "API Key is missing. Please configure your Gemini key in Settings.",
			})
			return
		}

		activeModel := getActiveModel(cfg)

		dbMutex.Lock()
		db := loadDB()
		history := db.Messages[req.SessionID]

		// Slice only the sliding window (e.g., last 10 messages) to save tokens
		windowLimit := cfg.SlidingWindow
		if windowLimit <= 0 {
			windowLimit = 10
		}
		var recentHistory []ChatMessage
		if len(history) > windowLimit {
			recentHistory = history[len(history)-windowLimit:]
		} else {
			recentHistory = history
		}
		dbMutex.Unlock()

		var contents []map[string]interface{}
		for _, m := range recentHistory {
			contents = append(contents, map[string]interface{}{
				"role": m.Role,
				"parts": []map[string]string{
					{"text": m.Content},
				},
			})
		}
		contents = append(contents, map[string]interface{}{
			"role": "user",
			"parts": []map[string]string{
				{"text": userText},
			},
		})

		systemPrompt := "You are Sahand CorText, a fast, sharp personal AI assistant and software architect. Provide clear, direct, and well-structured answers."

		reply, pTok, cTok, tTok, totTok, latency, err := callGeminiGenerate(cfg.GeminiAPIKey, activeModel, contents, systemPrompt)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		now := time.Now().Format("15:04:05")
		dbMutex.Lock()
		db = loadDB()

		userMsg := ChatMessage{
			ID:        db.NextMessageID,
			SessionID: req.SessionID,
			Role:      "user",
			Content:   userText,
			CreatedAt: now,
		}
		db.NextMessageID++

		aiMsg := ChatMessage{
			ID:              db.NextMessageID,
			SessionID:       req.SessionID,
			Role:            "model",
			Content:         reply,
			ModelUsed:       activeModel,
			PromptTokens:    pTok,
			CandidateTokens: cTok,
			ThoughtTokens:   tTok,
			TotalTokens:     totTok,
			LatencyMs:       latency,
			CreatedAt:       now,
		}
		db.NextMessageID++

		db.Messages[req.SessionID] = append(db.Messages[req.SessionID], userMsg, aiMsg)

		// Auto-title session on first user message & update session token count
		for i := range db.Sessions {
			if db.Sessions[i].ID == req.SessionID {
				if db.Sessions[i].Title == "New Chat" || db.Sessions[i].Title == "General Assistant" {
					title := userText
					if len(title) > 28 {
						title = title[:28] + "..."
					}
					db.Sessions[i].Title = title
				}
				db.Sessions[i].TotalTokens += totTok
				db.Sessions[i].UpdatedAt = time.Now().Format("2006-01-02 15:04")
				break
			}
		}

		todayStats := recordDailyUsage(&db, pTok, cTok, tTok, totTok)
		_ = saveDB(db)
		dbMutex.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":           true,
			"user_message": userMsg,
			"ai_message":   aiMsg,
			"sessions":     db.Sessions,
			"today_stats":  todayStats,
		})
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	go func() {
		_ = http.Serve(listener, mux)
	}()

	return fmt.Sprintf("http://%s", listener.Addr().String()), nil
}

func main() {
	serverURL, err := startLocalServer()
	if err != nil {
		return
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     true,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Sahand CorText — Personal AI Assistant",
			Width:  1120,
			Height: 740,
		},
	})

	if w == nil {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", serverURL).Start()
		select {}
	}

	defer w.Destroy()
	w.SetSize(1120, 740, webview2.HintNone)
	w.Navigate(serverURL)
	w.Run()
}

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Sahand CorText</title>
<style>
  :root {
    --bg: #0b1120;
    --panel: #111827;
    --card: #1e293b;
    --border: #334155;
    --text: #f8fafc;
    --muted: #94a3b8;
    --accent: #3b82f6;
    --accent-hover: #2563eb;
    --eco: #10b981;
    --work: #3b82f6;
    --deep: #a855f7;
    --danger: #ef4444;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: 'Segoe UI', system-ui, sans-serif; }
  body { background: var(--bg); color: var(--text); display: flex; height: 100vh; overflow: hidden; }

  /* Sidebar */
  .sidebar {
    width: 260px;
    background: #070b14;
    border-right: 1px solid var(--border);
    display: flex;
    flex-direction: column;
    padding: 18px 14px;
  }
  .brand {
    font-size: 17px;
    font-weight: 700;
    color: #fff;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 4px 6px 16px;
    border-bottom: 1px solid #1e293b;
  }
  .brand-dot {
    width: 10px; height: 10px; border-radius: 50%;
    background: var(--accent); box-shadow: 0 0 10px var(--accent);
  }
  .new-chat-btn {
    margin-top: 14px;
    width: 100%;
    padding: 10px 12px;
    background: var(--accent);
    color: #fff;
    border: none;
    border-radius: 8px;
    font-weight: 600;
    font-size: 13px;
    cursor: pointer;
    transition: background 0.15s;
  }
  .new-chat-btn:hover { background: var(--accent-hover); }
  .section-label {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.8px;
    color: var(--muted);
    margin: 18px 6px 8px;
    font-weight: 600;
  }
  .session-list {
    flex: 1;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .session-item {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 9px 10px;
    border-radius: 8px;
    font-size: 13px;
    color: #cbd5e1;
    cursor: pointer;
    border: 1px solid transparent;
  }
  .session-item:hover { background: #111827; }
  .session-item.active {
    background: #1e293b;
    border-color: var(--border);
    color: #fff;
    font-weight: 600;
  }
  .session-title {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 165px;
  }
  .session-meta {
    font-size: 11px;
    color: var(--muted);
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .del-btn {
    background: transparent;
    border: none;
    color: var(--muted);
    cursor: pointer;
    font-size: 14px;
    padding: 0 4px;
  }
  .del-btn:hover { color: var(--danger); }

  .sidebar-footer {
    border-top: 1px solid #1e293b;
    padding-top: 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .nav-btn {
    width: 100%;
    padding: 10px 12px;
    background: #111827;
    border: 1px solid var(--border);
    color: #e2e8f0;
    border-radius: 8px;
    font-size: 13px;
    font-weight: 600;
    cursor: pointer;
    text-align: left;
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  .nav-btn:hover { border-color: var(--accent); }

  /* Main Area */
  .main {
    flex: 1;
    display: flex;
    flex-direction: column;
    height: 100vh;
    overflow: hidden;
  }

  /* Top Header: Tier Switcher & Token Ledger */
  .topbar {
    background: #0f172a;
    border-bottom: 1px solid var(--border);
    padding: 12px 22px;
    display: flex;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
    gap: 12px;
  }
  .tier-group {
    display: flex;
    align-items: center;
    gap: 6px;
    background: #070b14;
    padding: 4px;
    border-radius: 10px;
    border: 1px solid var(--border);
  }
  .tier-btn {
    padding: 7px 12px;
    border: none;
    background: transparent;
    color: var(--muted);
    font-size: 12px;
    font-weight: 600;
    border-radius: 7px;
    cursor: pointer;
    transition: all 0.15s;
  }
  .tier-btn:hover { color: #fff; }
  .tier-btn.active-eco { background: rgba(16, 185, 129, 0.2); color: #34d399; border: 1px solid rgba(16,185,129,0.4); }
  .tier-btn.active-workhorse { background: rgba(59, 130, 246, 0.2); color: #60a5fa; border: 1px solid rgba(59,130,246,0.4); }
  .tier-btn.active-deep { background: rgba(168, 85, 247, 0.2); color: #c084fc; border: 1px solid rgba(168,85,247,0.4); }
  .tier-btn.active-custom { background: rgba(245, 158, 11, 0.2); color: #fbbf24; border: 1px solid rgba(245,158,11,0.4); }

  .active-model-pill {
    font-size: 12px;
    color: #cbd5e1;
    font-family: Consolas, monospace;
    background: #1e293b;
    padding: 5px 10px;
    border-radius: 6px;
    border: 1px solid var(--border);
  }

  .ledger {
    display: flex;
    gap: 14px;
    align-items: center;
    font-size: 12px;
  }
  .ledger-box {
    background: #1e293b;
    border: 1px solid var(--border);
    padding: 5px 11px;
    border-radius: 8px;
    color: #cbd5e1;
  }
  .ledger-box b { color: #fff; font-family: Consolas, monospace; }

  /* Chat View */
  #chatView {
    flex: 1;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }
  .messages-container {
    flex: 1;
    overflow-y: auto;
    padding: 24px 32px;
    display: flex;
    flex-direction: column;
    gap: 18px;
  }
  .msg-row {
    display: flex;
    flex-direction: column;
    max-width: 82%;
  }
  .msg-row.user { align-self: flex-end; align-items: flex-end; }
  .msg-row.model { align-self: flex-start; align-items: flex-start; }

  .msg-bubble {
    padding: 14px 18px;
    border-radius: 12px;
    font-size: 14px;
    line-height: 1.6;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .msg-row.user .msg-bubble {
    background: var(--accent);
    color: #fff;
    border-bottom-right-radius: 3px;
  }
  .msg-row.model .msg-bubble {
    background: var(--card);
    border: 1px solid var(--border);
    color: var(--text);
    border-bottom-left-radius: 3px;
  }
  .msg-telemetry {
    margin-top: 5px;
    font-size: 11px;
    color: var(--muted);
    font-family: Consolas, monospace;
    display: flex;
    gap: 10px;
  }

  /* Input Composer */
  .composer {
    padding: 14px 28px 18px;
    background: #0f172a;
    border-top: 1px solid var(--border);
    display: flex;
    gap: 10px;
    align-items: flex-end;
  }
  .composer textarea {
    flex: 1;
    background: #070b14;
    border: 1px solid var(--border);
    border-radius: 10px;
    color: #fff;
    padding: 12px 14px;
    font-size: 14px;
    resize: none;
    height: 52px;
    outline: none;
    line-height: 1.4;
  }
  .composer textarea:focus { border-color: var(--accent); }
  .send-btn {
    height: 52px;
    padding: 0 24px;
    background: var(--accent);
    color: #fff;
    border: none;
    border-radius: 10px;
    font-weight: 700;
    font-size: 14px;
    cursor: pointer;
  }
  .send-btn:hover { background: var(--accent-hover); }
  .send-btn:disabled { opacity: 0.5; cursor: not-allowed; }

  /* Settings View */
  #settingsView {
    flex: 1;
    overflow-y: auto;
    padding: 36px;
    display: none;
    justify-content: center;
    align-items: flex-start;
  }
  .settings-card {
    width: 100%;
    max-width: 620px;
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 28px;
  }
  .settings-card h2 { font-size: 19px; margin-bottom: 6px; }
  .settings-card p { font-size: 13px; color: var(--muted); margin-bottom: 20px; line-height: 1.5; }
  label { display: block; font-size: 12px; font-weight: 600; color: #cbd5e1; margin-bottom: 6px; margin-top: 14px; }
  input, select {
    width: 100%;
    padding: 10px 12px;
    background: #0b1120;
    border: 1px solid var(--border);
    border-radius: 8px;
    color: #fff;
    font-size: 13px;
  }
  .row-flex { display: flex; gap: 8px; }
  .btn-primary {
    margin-top: 20px;
    width: 100%;
    padding: 12px;
    background: var(--accent);
    color: #fff;
    border: none;
    border-radius: 8px;
    font-weight: 600;
    cursor: pointer;
  }
  .tier-preview {
    margin-top: 16px;
    padding: 12px;
    background: #0b1120;
    border: 1px solid var(--border);
    border-radius: 8px;
    font-size: 12px;
    font-family: Consolas, monospace;
    line-height: 1.7;
    color: #cbd5e1;
  }
</style>
</head>
<body>
  <!-- LEFT SIDEBAR -->
  <div class="sidebar">
    <div>
      <div class="brand"><span class="brand-dot"></span> sahand-cortext</div>
      <button class="new-chat-btn" onclick="createNewSession()">+ New Chat Session</button>
      <div class="section-label">Conversations</div>
    </div>

    <div id="sessionList" class="session-list"></div>

    <div class="sidebar-footer">
      <button class="nav-btn" onclick="toggleView('chat')">
        <span>💬 Chat Workspace</span>
      </button>
      <button class="nav-btn" onclick="toggleView('settings')">
        <span>⚙️ API & Tier Settings</span>
        <span id="keyStatusDot" style="color: #10b981;">●</span>
      </button>
    </div>
  </div>

  <!-- MAIN AREA -->
  <div class="main">
    <!-- TOP BAR -->
    <div class="topbar">
      <div style="display:flex; align-items:center; gap:10px;">
        <div class="tier-group">
          <button id="btnEco" class="tier-btn" onclick="switchTier('eco')" title="Cheapest & Fastest Lite Model">⚡ Eco (Lite)</button>
          <button id="btnWork" class="tier-btn" onclick="switchTier('workhorse')" title="Balanced Flash Model">🚀 Workhorse (Flash)</button>
          <button id="btnDeep" class="tier-btn" onclick="switchTier('deep')" title="Deep Reasoning Argon Model (High Token Burn)">🧠 Deep (Argon)</button>
          <button id="btnCustom" class="tier-btn" onclick="switchTier('custom')" title="Use Custom Selected Model">🎯 Custom</button>
        </div>
        <span id="activeModelPill" class="active-model-pill">gemini-3.8-flash</span>
      </div>

      <div class="ledger">
        <div class="ledger-box" title="Tokens used in the currently open conversation">
          Session: <b id="sessionTokens">0</b> tok
        </div>
        <div class="ledger-box" title="Total API calls and tokens used today across all chats">
          Today: <b id="todayReqs">0</b> reqs • <b id="todayTokens">0</b> tok (<span id="todayThoughts" style="color:#c084fc">0</span> thought)
        </div>
      </div>
    </div>

    <!-- CHAT WORKSPACE VIEW -->
    <div id="chatView">
      <div id="messagesBox" class="messages-container"></div>

      <div class="composer">
        <textarea id="chatInput" placeholder="Message Sahand CorText... (Enter to send, Shift+Enter for newline)"></textarea>
        <button id="sendBtn" class="send-btn" onclick="sendMessage()">Send</button>
      </div>
    </div>

    <!-- SETTINGS VIEW -->
    <div id="settingsView">
      <div class="settings-card">
        <h2>API Key & Auto-Tier Configuration</h2>
        <p>Configure your Gemini API key and choose which performance tier boots automatically when you open Sahand CorText so you never burn Argon tokens accidentally.</p>

        <label>Gemini API Key</label>
        <div class="row-flex">
          <input type="password" id="apiKeyInput" placeholder="AIzaSy..." />
          <button class="nav-btn" style="width:auto;" onclick="togglePassword()">Show</button>
        </div>

        <label>Default Startup Mode (When App Opens)</label>
        <select id="startupTierSelect" onchange="saveTierPreferences()">
          <option value="eco">⚡ Eco / Cheapest (gemini-3.8-flash-lite)</option>
          <option value="workhorse">🚀 Workhorse — Recommended Default (gemini-3.8-flash)</option>
          <option value="deep">🧠 Deep Thinker (gemini-4-argon)</option>
          <option value="custom">🎯 Custom Specific Model</option>
        </select>

        <label>Custom Model Override (Used when 🎯 Custom is active)</label>
        <select id="customModelSelect" onchange="saveTierPreferences()"></select>

        <label>Sliding Context Window (Messages sent per turn to save tokens)</label>
        <select id="windowSelect" onchange="saveTierPreferences()">
          <option value="6">Last 6 messages (Ultra-Lean Token Saver)</option>
          <option value="10">Last 10 messages (Balanced — Recommended)</option>
          <option value="20">Last 20 messages (Deep Context)</option>
        </select>

        <div id="tierPreviewBox" class="tier-preview"></div>

        <button id="verifyBtn" class="btn-primary" onclick="verifyAndDiscover()">↻ Auto-Discover Models & Verify Key</button>
        <div id="verifyStatus" style="margin-top:12px; font-size:13px;"></div>
      </div>
    </div>
  </div>

<script>
  let appState = {
    config: {},
    activeModel: 'gemini-3.8-flash',
    sessions: [],
    currentSessionId: 1,
    todayStats: {}
  };

  window.addEventListener('DOMContentLoaded', async () => {
    const input = document.getElementById('chatInput');
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        sendMessage();
      }
    });
    await loadAppState();
  });

  async function loadAppState() {
    const res = await fetch('/api/state');
    const data = await res.json();
    appState.config = data.config;
    appState.activeModel = data.active_model;
    appState.sessions = data.sessions || [];
    appState.todayStats = data.today_stats || {};

    if (appState.sessions.length > 0) {
      appState.currentSessionId = appState.sessions[0].id;
    }

    syncUI();

    if (!appState.config.verified || !appState.config.gemini_api_key) {
      toggleView('settings');
    } else {
      toggleView('chat');
      await loadMessages(appState.currentSessionId);
    }
  }

  function syncUI() {
    const cfg = appState.config;
    // Topbar tier buttons
    document.getElementById('btnEco').className = 'tier-btn' + (cfg.active_tier === 'eco' ? ' active-eco' : '');
    document.getElementById('btnWork').className = 'tier-btn' + (cfg.active_tier === 'workhorse' ? ' active-workhorse' : '');
    document.getElementById('btnDeep').className = 'tier-btn' + (cfg.active_tier === 'deep' ? ' active-deep' : '');
    document.getElementById('btnCustom').className = 'tier-btn' + (cfg.active_tier === 'custom' ? ' active-custom' : '');

    document.getElementById('activeModelPill').textContent = appState.activeModel;

    // Daily stats
    const st = appState.todayStats || {};
    document.getElementById('todayReqs').textContent = (st.requests_count || 0).toLocaleString();
    document.getElementById('todayTokens').textContent = (st.total_tokens || 0).toLocaleString();
    document.getElementById('todayThoughts').textContent = (st.thought_tokens || 0).toLocaleString();

    // Current session tokens
    const currSess = appState.sessions.find(s => s.id === appState.currentSessionId);
    document.getElementById('sessionTokens').textContent = currSess ? (currSess.total_tokens || 0).toLocaleString() : '0';

    // Sidebar sessions
    const list = document.getElementById('sessionList');
    list.innerHTML = '';
    appState.sessions.forEach(s => {
      const div = document.createElement('div');
      div.className = 'session-item' + (s.id === appState.currentSessionId ? ' active' : '');
      div.onclick = () => selectSession(s.id);

      const titleSpan = document.createElement('span');
      titleSpan.className = 'session-title';
      titleSpan.textContent = s.title;

      const meta = document.createElement('div');
      meta.className = 'session-meta';
      const tokSpan = document.createElement('span');
      tokSpan.textContent = (s.total_tokens || 0) + 't';

      const delBtn = document.createElement('button');
      delBtn.className = 'del-btn';
      delBtn.textContent = '×';
      delBtn.title = 'Delete chat';
      delBtn.onclick = (ev) => {
        ev.stopPropagation();
        deleteSession(s.id);
      };

      meta.appendChild(tokSpan);
      meta.appendChild(delBtn);
      div.appendChild(titleSpan);
      div.appendChild(meta);
      list.appendChild(div);
    });

    // Settings inputs
    document.getElementById('apiKeyInput').value = cfg.gemini_api_key || '';
    document.getElementById('startupTierSelect').value = cfg.startup_tier || 'workhorse';
    document.getElementById('windowSelect').value = String(cfg.sliding_window || 10);
    document.getElementById('keyStatusDot').style.color = cfg.verified ? '#10b981' : '#ef4444';

    const customSel = document.getElementById('customModelSelect');
    customSel.innerHTML = '';
    (cfg.available_models || []).forEach(m => {
      const opt = document.createElement('option');
      opt.value = m.id;
      opt.textContent = m.display_name || m.id;
      customSel.appendChild(opt);
    });
    if (cfg.custom_model) {
      customSel.value = cfg.custom_model;
    }

    const tm = cfg.tier_map || {};
    document.getElementById('tierPreviewBox').innerHTML =
      '<b>Auto-Resolved Tier Mapping:</b><br>' +
      '⚡ Eco (Cheapest): ' + (tm.eco || 'gemini-3.8-flash-lite') + '<br>' +
      '🚀 Workhorse: ' + (tm.workhorse || 'gemini-3.8-flash') + '<br>' +
      '🧠 Deep Thinker: ' + (tm.deep || 'gemini-4-argon');
  }

  function toggleView(view) {
    document.getElementById('chatView').style.display = (view === 'chat') ? 'flex' : 'none';
    document.getElementById('settingsView').style.display = (view === 'settings') ? 'flex' : 'none';
  }

  async function switchTier(tier) {
    const res = await fetch('/api/set-tier', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ active_tier: tier })
    });
    const data = await res.json();
    if (data.ok) {
      appState.config = data.config;
      appState.activeModel = data.active_model;
      syncUI();
    }
  }

  async function saveTierPreferences() {
    const startupTier = document.getElementById('startupTierSelect').value;
    const customModel = document.getElementById('customModelSelect').value;
    const slidingWindow = parseInt(document.getElementById('windowSelect').value, 10);

    const res = await fetch('/api/set-tier', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        startup_tier: startupTier,
        custom_model: customModel,
        sliding_window: slidingWindow
      })
    });
    const data = await res.json();
    if (data.ok) {
      appState.config = data.config;
      appState.activeModel = data.active_model;
      syncUI();
    }
  }

  async function selectSession(id) {
    appState.currentSessionId = id;
    toggleView('chat');
    syncUI();
    await loadMessages(id);
  }

  async function createNewSession() {
    const res = await fetch('/api/session/new', { method: 'POST' });
    const data = await res.json();
    appState.sessions = data.sessions;
    appState.currentSessionId = data.session.id;
    toggleView('chat');
    syncUI();
    await loadMessages(appState.currentSessionId);
  }

  async function deleteSession(id) {
    const res = await fetch('/api/session/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: id })
    });
    const data = await res.json();
    appState.sessions = data.sessions;
    appState.currentSessionId = appState.sessions[0].id;
    syncUI();
    await loadMessages(appState.currentSessionId);
  }

  async function loadMessages(sessionId) {
    const res = await fetch('/api/messages?session_id=' + sessionId);
    const data = await res.json();
    const box = document.getElementById('messagesBox');
    box.innerHTML = '';

    const msgs = data.messages || [];
    if (msgs.length === 0) {
      box.innerHTML = '<div style="color:#64748b; text-align:center; margin-top:60px; font-size:14px;">' +
        'Start chatting with <b>' + appState.activeModel + '</b>.<br>Switch between ⚡ Eco, 🚀 Workhorse, and 🧠 Deep Thinker anytime in the top bar.</div>';
      return;
    }

    msgs.forEach(m => appendMessageElement(m));
    box.scrollTop = box.scrollHeight;
  }

  function appendMessageElement(m) {
    const box = document.getElementById('messagesBox');
    const row = document.createElement('div');
    row.className = 'msg-row ' + m.role;

    const bubble = document.createElement('div');
    bubble.className = 'msg-bubble';
    bubble.textContent = m.content;
    row.appendChild(bubble);

    if (m.role === 'model') {
      const meta = document.createElement('div');
      meta.className = 'msg-telemetry';
      let thoughtText = m.thought_tokens ? (' • 🧠 ' + m.thought_tokens + ' thought') : '';
      meta.textContent = m.model_used + ' • ' + m.latency_ms + 'ms • ' +
        m.total_tokens + ' tok (in:' + m.prompt_tokens + ' / out:' + m.candidate_tokens + thoughtText + ')';
      row.appendChild(meta);
    }

    box.appendChild(row);
    box.scrollTop = box.scrollHeight;
  }

  async function sendMessage() {
    const input = document.getElementById('chatInput');
    const btn = document.getElementById('sendBtn');
    const text = input.value.trim();
    if (!text) return;

    input.value = '';
    btn.disabled = true;
    btn.textContent = 'Thinking...';

    // Remove placeholder if first message
    const box = document.getElementById('messagesBox');
    if (box.children.length === 1 && box.children[0].tagName === 'DIV' && !box.children[0].classList.contains('msg-row')) {
      box.innerHTML = '';
    }

    appendMessageElement({ role: 'user', content: text });

    try {
      const res = await fetch('/api/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: appState.currentSessionId,
          text: text
        })
      });
      const data = await res.json();
      if (!data.ok) {
        appendMessageElement({
          role: 'model',
          content: '⚠️ Error: ' + data.error,
          model_used: appState.activeModel,
          latency_ms: 0,
          total_tokens: 0,
          prompt_tokens: 0,
          candidate_tokens: 0
        });
      } else {
        appendMessageElement(data.ai_message);
        appState.sessions = data.sessions;
        appState.todayStats = data.today_stats;
        syncUI();
      }
    } catch (err) {
      appendMessageElement({
        role: 'model',
        content: '⚠️ Local connection error: ' + err.message,
        model_used: 'system',
        latency_ms: 0,
        total_tokens: 0,
        prompt_tokens: 0,
        candidate_tokens: 0
      });
    } finally {
      btn.disabled = false;
      btn.textContent = 'Send';
      input.focus();
    }
  }

  function togglePassword() {
    const el = document.getElementById('apiKeyInput');
    el.type = (el.type === 'password') ? 'text' : 'password';
  }

  async function verifyAndDiscover() {
    const apiKey = document.getElementById('apiKeyInput').value.trim();
    const startupTier = document.getElementById('startupTierSelect').value;
    const customModel = document.getElementById('customModelSelect').value;
    const btn = document.getElementById('verifyBtn');
    const status = document.getElementById('verifyStatus');

    btn.disabled = true;
    btn.textContent = 'Scanning Models & Verifying...';
    status.style.color = '#94a3b8';
    status.textContent = 'Contacting Google Gemini servers...';

    try {
      const res = await fetch('/api/verify-gemini', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          api_key: apiKey,
          startup_tier: startupTier,
          custom_model: customModel
        })
      });
      const data = await res.json();
      if (data.valid) {
        appState.config = data.config;
        appState.activeModel = data.model_used;
        appState.todayStats = data.today_stats;
        syncUI();
        status.style.color = '#10b981';
        status.textContent = '✔ ' + data.message + ' (' + data.latency_ms + ' ms)';
        setTimeout(() => toggleView('chat'), 900);
      } else {
        status.style.color = '#ef4444';
        status.textContent = '✖ ' + data.message;
      }
    } catch (e) {
      status.style.color = '#ef4444';
      status.textContent = '✖ Error: ' + e.message;
    } finally {
      btn.disabled = false;
      btn.textContent = '↻ Auto-Discover Models & Verify Key';
    }
  }
</script>
</body>
</html>
`
