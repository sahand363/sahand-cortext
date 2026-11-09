package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// desktopLauncher is populated automatically on Windows builds to open WebView2
var desktopLauncher func(serverURL string)

// ModelInfo holds discovered model metadata and cumulative self-learning telemetry
type ModelInfo struct {
	ID               string  `json:"id"`
	DisplayName      string  `json:"display_name"`
	Family           string  `json:"family"` // "lite", "flash", "deep", "other"
	Version          float64 `json:"version"`
	VerifiedWorking  bool    `json:"verified_working"`
	SuccessCount     int     `json:"success_count"`
	FailCount        int     `json:"fail_count"`
	ConsecutiveFails int     `json:"consecutive_fails"`
	AvgLatencyMs     int64   `json:"avg_latency_ms"`
	LastError        string  `json:"last_error"`
	LastTestedAt     string  `json:"last_tested_at"`
	SmartScore       float64 `json:"smart_score"`
}

// AppConfig stores local & server hub settings
type AppConfig struct {
	GeminiAPIKey    string      `json:"gemini_api_key"`
	ActiveModel     string      `json:"active_model"`
	AutoFailover    bool        `json:"auto_failover"`
	TopModels       []ModelInfo `json:"top_models"`       // Top 5 Proven Working Models
	AvailableModels []ModelInfo `json:"available_models"` // Full discovered & scored list
	SlidingWindow   int         `json:"sliding_window"`
	Verified        bool        `json:"verified"`
	LastTestedAt    string      `json:"last_tested_at"`

	// Remote Ubuntu VPS Hub Settings (for Desktop client)
	UseRemoteServer bool   `json:"use_remote_server"`
	RemoteServerURL string `json:"remote_server_url"`
	RemoteSecret    string `json:"remote_secret"`
}

type ChatMessage struct {
	ID              int64  `json:"id"`
	SessionID       int64  `json:"session_id"`
	Role            string `json:"role"`
	Content         string `json:"content"`
	ModelUsed       string `json:"model_used"`
	PromptTokens    int    `json:"prompt_tokens"`
	CandidateTokens int    `json:"candidate_tokens"`
	ThoughtTokens   int    `json:"thought_tokens"`
	TotalTokens     int    `json:"total_tokens"`
	LatencyMs       int64  `json:"latency_ms"`
	FailoverNote    string `json:"failover_note,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type ChatSession struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	TotalTokens int    `json:"total_tokens"`
	UpdatedAt   string `json:"updated_at"`
}

type DailyStats struct {
	Date            string `json:"date"`
	RequestsCount   int    `json:"requests_count"`
	PromptTokens    int    `json:"prompt_tokens"`
	CandidateTokens int    `json:"candidate_tokens"`
	ThoughtTokens   int    `json:"thought_tokens"`
	TotalTokens     int    `json:"total_tokens"`
}

type LocalDatabase struct {
	NextSessionID  int64                   `json:"next_session_id"`
	NextMessageID  int64                   `json:"next_message_id"`
	Sessions       []ChatSession           `json:"sessions"`
	Messages       map[int64][]ChatMessage `json:"messages"`
	DailyUsage     map[string]DailyStats   `json:"daily_usage"`
	ModelTelemetry map[string]ModelInfo    `json:"model_telemetry"`
}

var (
	dbMutex      sync.Mutex
	serverSecret string
	versionRegex = regexp.MustCompile(`(\d+\.\d+|\d+)`)
)

func getAppDir() string {
	appData, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(appData, "SahandCorText")
	_ = os.MkdirAll(dir, 0700)
	return dir
}

func getConfigPath() string { return filepath.Join(getAppDir(), "config.json") }
func getDBPath() string     { return filepath.Join(getAppDir(), "cortext_db.json") }

// parseVersionAndFamily extracts semantic version (e.g. 3.8 > 3.5) and family ("lite", "flash", "deep")
func parseVersionAndFamily(id string) (float64, string) {
	lower := strings.ToLower(id)
	ver := 3.0
	matches := versionRegex.FindAllString(lower, -1)
	if len(matches) > 0 {
		if v, err := strconv.ParseFloat(matches[0], 64); err == nil {
			ver = v
		}
	}

	family := "other"
	switch {
	case strings.Contains(lower, "lite"):
		family = "lite"
	case strings.Contains(lower, "flash"):
		family = "flash"
	case strings.Contains(lower, "argon") || strings.Contains(lower, "pro") || strings.Contains(lower, "ultra"):
		family = "deep"
	}
	return ver, family
}

// computeSmartScore ranks models by:
// 1. Proven Working Status
// 2. Reliability Ratio (Successes vs Fails)
// 3. Higher Version Priority (e.g., 3.8 Lite strictly beats 3.5 Lite)
// 4. Real-World Speed (AvgLatencyMs)
func computeSmartScore(m ModelInfo) float64 {
	score := m.Version * 150.0 // e.g. 3.8 -> 570 pts, 3.5 -> 525 pts, 4.0 -> 600 pts

	// Family base preference (prioritize Lite & Flash for reliable everyday connectivity)
	switch m.Family {
	case "lite":
		score += 90.0
	case "flash":
		score += 75.0
	case "deep":
		score += 40.0
	}

	// Prefer clean stable IDs over experimental/dated suffixes
	if !strings.Contains(m.ID, "preview") && !strings.Contains(m.ID, "exp") && !strings.Contains(m.ID, "-00") {
		score += 25.0
	}

	totalAttempts := m.SuccessCount + m.FailCount
	if m.VerifiedWorking && m.SuccessCount > 0 {
		reliability := float64(m.SuccessCount) / float64(totalAttempts)
		score += 1000.0 + (reliability * 500.0)

		// Reward cumulative proven stability (up to +150 pts)
		experienceBonus := math.Min(float64(m.SuccessCount)*10.0, 150.0)
		score += experienceBonus

		// Speed bonus: faster models get up to +120 pts
		if m.AvgLatencyMs > 0 {
			speedBonus := math.Max(0, 120.0-(float64(m.AvgLatencyMs)/35.0))
			score += speedBonus
		}
	}

	// Heavy penalty for consecutive failures or broken endpoints
	if m.ConsecutiveFails > 0 {
		score -= float64(m.ConsecutiveFails) * 600.0
	}
	if !m.VerifiedWorking && m.FailCount > 0 {
		score -= 800.0
	}

	return math.Round(score*10) / 10
}

func defaultSeedModels() []ModelInfo {
	seeds := []string{
		"gemini-3.8-flash-lite",
		"gemini-3.8-flash",
		"gemini-3.5-flash-lite",
		"gemini-3.5-flash",
		"gemini-4-argon",
	}
	var list []ModelInfo
	for _, id := range seeds {
		ver, fam := parseVersionAndFamily(id)
		m := ModelInfo{
			ID:          id,
			DisplayName: id,
			Version:     ver,
			Family:      fam,
		}
		m.SmartScore = computeSmartScore(m)
		list = append(list, m)
	}
	return list
}

func isExcludedModel(id string) bool {
	lower := strings.ToLower(id)
	excluded := []string{
		"tts", "audio", "embedding", "imagen", "veo", "aqa",
		"bisheng", "learnlm", "robotics", "computer-use",
		"1.5", "2.0", "2.5", "live", "thinking-exp", "vision",
	}
	for _, kw := range excluded {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func loadDB() LocalDatabase {
	db := LocalDatabase{
		NextSessionID:  1,
		NextMessageID:  1,
		Sessions:       []ChatSession{},
		Messages:       make(map[int64][]ChatMessage),
		DailyUsage:     make(map[string]DailyStats),
		ModelTelemetry: make(map[string]ModelInfo),
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
	if db.ModelTelemetry == nil {
		db.ModelTelemetry = make(map[string]ModelInfo)
	}
	if len(db.Sessions) == 0 {
		db.Sessions = append(db.Sessions, ChatSession{
			ID:        1,
			Title:     "General Assistant",
			UpdatedAt: time.Now().Format("2006-01-02 15:04"),
		})
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

// rebuildRankedModels merges discovered models with DB telemetry and selects the Top 5
func rebuildRankedModels(cfg *AppConfig, db *LocalDatabase) {
	seen := make(map[string]bool)
	var all []ModelInfo

	for _, m := range cfg.AvailableModels {
		if isExcludedModel(m.ID) {
			continue
		}
		seen[m.ID] = true
		if tele, ok := db.ModelTelemetry[m.ID]; ok {
			tele.DisplayName = m.DisplayName
			tele.Version, tele.Family = parseVersionAndFamily(m.ID)
			tele.SmartScore = computeSmartScore(tele)
			all = append(all, tele)
		} else {
			m.Version, m.Family = parseVersionAndFamily(m.ID)
			m.SmartScore = computeSmartScore(m)
			all = append(all, m)
		}
	}

	for id, tele := range db.ModelTelemetry {
		if !seen[id] && !isExcludedModel(id) {
			tele.Version, tele.Family = parseVersionAndFamily(id)
			tele.SmartScore = computeSmartScore(tele)
			all = append(all, tele)
		}
	}

	if len(all) == 0 {
		all = defaultSeedModels()
	}

	// Sort by SmartScore descending
	sort.Slice(all, func(i, j int) bool {
		if all[i].SmartScore != all[j].SmartScore {
			return all[i].SmartScore > all[j].SmartScore
		}
		return all[i].ID < all[j].ID
	})

	cfg.AvailableModels = all

	// Build Top 5: prefer verified working models first, then highest-scored candidates
	var top5 []ModelInfo
	for _, m := range all {
		if m.VerifiedWorking && m.ConsecutiveFails < 3 {
			top5 = append(top5, m)
			if len(top5) == 5 {
				break
			}
		}
	}
	if len(top5) < 5 {
		for _, m := range all {
			already := false
			for _, t := range top5 {
				if t.ID == m.ID {
					already = true
					break
				}
			}
			if !already && m.ConsecutiveFails == 0 {
				top5 = append(top5, m)
				if len(top5) == 5 {
					break
				}
			}
		}
	}

	cfg.TopModels = top5

	// Ensure ActiveModel is valid and defaults to #1 best working model
	activeValid := false
	for _, m := range top5 {
		if m.ID == cfg.ActiveModel {
			activeValid = true
			break
		}
	}
	if (!activeValid || cfg.ActiveModel == "") && len(top5) > 0 {
		cfg.ActiveModel = top5[0].ID
	}
}

func loadConfig() AppConfig {
	cfg := AppConfig{
		ActiveModel:     "gemini-3.8-flash-lite",
		AutoFailover:    true,
		AvailableModels: defaultSeedModels(),
		SlidingWindow:   10,
	}
	data, err := os.ReadFile(getConfigPath())
	if err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	if envKey := os.Getenv("GEMINI_API_KEY"); envKey != "" && cfg.GeminiAPIKey == "" {
		cfg.GeminiAPIKey = envKey
		cfg.Verified = true
	}
	if cfg.SlidingWindow <= 0 {
		cfg.SlidingWindow = 10
	}
	db := loadDB()
	rebuildRankedModels(&cfg, &db)
	return cfg
}

func saveConfig(cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0600)
}

// recordModelOutcome updates the self-learning telemetry after every probe or chat call
func recordModelOutcome(db *LocalDatabase, modelID string, success bool, latencyMs int64, errMsg string) {
	tele, exists := db.ModelTelemetry[modelID]
	if !exists {
		ver, fam := parseVersionAndFamily(modelID)
		tele = ModelInfo{
			ID:          modelID,
			DisplayName: modelID,
			Version:     ver,
			Family:      fam,
		}
	}
	tele.Version, tele.Family = parseVersionAndFamily(modelID)
	tele.LastTestedAt = time.Now().Format("15:04:05")

	if success {
		tele.VerifiedWorking = true
		tele.SuccessCount++
		tele.ConsecutiveFails = 0
		tele.LastError = ""
		if tele.AvgLatencyMs == 0 {
			tele.AvgLatencyMs = latencyMs
		} else {
			// Exponential moving average (70% historical, 30% latest)
			tele.AvgLatencyMs = int64(float64(tele.AvgLatencyMs)*0.7 + float64(latencyMs)*0.3)
		}
	} else {
		tele.FailCount++
		tele.ConsecutiveFails++
		tele.LastError = errMsg
		if tele.ConsecutiveFails >= 3 {
			tele.VerifiedWorking = false
		}
	}
	tele.SmartScore = computeSmartScore(tele)
	db.ModelTelemetry[modelID] = tele
}

func fetchCatalogModels(apiKey string) ([]ModelInfo, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("API key is empty")
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s&pageSize=100", apiKey)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from Google catalog", resp.StatusCode)
	}

	var parsed struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
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
		ver, fam := parseVersionAndFamily(id)
		info := ModelInfo{
			ID:          id,
			DisplayName: id,
			Version:     ver,
			Family:      fam,
		}
		info.SmartScore = computeSmartScore(info)
		list = append(list, info)
	}
	return list, nil
}

func callGeminiGenerate(apiKey, model string, contents []map[string]interface{}, systemPrompt string, timeout time.Duration) (string, int, int, int, int, int64, error) {
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
	client := &http.Client{Timeout: timeout}

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
		errMsg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if e, ok := errObj["error"].(map[string]interface{}); ok {
			if m, ok := e["message"].(string); ok {
				errMsg = m
			}
		}
		return "", 0, 0, 0, 0, latency, fmt.Errorf("%s", errMsg)
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
		reply = "OK"
	}

	u := parsed.UsageMetadata
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount + u.ThoughtsTokenCount
	}
	return reply, u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount, total, latency, nil
}

// runSmartBenchmark probes candidate models in parallel to discover which ones actually connect
// and prioritizes higher versions (e.g. 3.8 > 3.5)
func runSmartBenchmark(apiKey string) ([]ModelInfo, int, error) {
	catalog, err := fetchCatalogModels(apiKey)
	if err != nil || len(catalog) == 0 {
		catalog = defaultSeedModels()
	}

	// Sort candidates so highest version Lite & Flash models are tested first
	sort.Slice(catalog, func(i, j int) bool {
		return catalog[i].SmartScore > catalog[j].SmartScore
	})

	// Pick up to 10 best candidates (prioritizing clean Lite, Flash, and Deep models)
	var candidates []ModelInfo
	for _, m := range catalog {
		// Skip obscure dated snapshots if we already have enough candidates
		if len(candidates) >= 10 {
			break
		}
		if strings.Contains(m.ID, "lite") || strings.Contains(m.ID, "flash") || strings.Contains(m.ID, "argon") || strings.Contains(m.ID, "pro") {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		candidates = catalog
		if len(candidates) > 8 {
			candidates = candidates[:8]
		}
	}

	type probeResult struct {
		id      string
		ok      bool
		latency int64
		pTok    int
		cTok    int
		tTok    int
		totTok  int
		errMsg  string
	}

	resultsChan := make(chan probeResult, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // Max 4 concurrent probes so we don't trigger RPM limits

	testContents := []map[string]interface{}{
		{
			"role": "user",
			"parts": []map[string]string{
				{"text": "Reply OK"},
			},
		},
	}

	for _, cand := range candidates {
		wg.Add(1)
		go func(modelID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			_, pTok, cTok, tTok, totTok, lat, err := callGeminiGenerate(apiKey, modelID, testContents, "", 12*time.Second)
			if err != nil {
				resultsChan <- probeResult{id: modelID, ok: false, latency: lat, errMsg: err.Error()}
			} else {
				resultsChan <- probeResult{id: modelID, ok: true, latency: lat, pTok: pTok, cTok: cTok, tTok: tTok, totTok: totTok}
			}
		}(cand.ID)
	}

	wg.Wait()
	close(resultsChan)

	dbMutex.Lock()
	defer dbMutex.Unlock()

	db := loadDB()
	cfg := loadConfig()
	cfg.GeminiAPIKey = apiKey
	cfg.AvailableModels = catalog

	workingCount := 0
	for res := range resultsChan {
		recordModelOutcome(&db, res.id, res.ok, res.latency, res.errMsg)
		if res.ok {
			workingCount++
			recordDailyUsage(&db, res.pTok, res.cTok, res.tTok, res.totTok)
		}
	}

	rebuildRankedModels(&cfg, &db)
	if workingCount > 0 {
		cfg.Verified = true
		cfg.LastTestedAt = time.Now().Format("2006-01-02 15:04:05")
		if len(cfg.TopModels) > 0 {
			cfg.ActiveModel = cfg.TopModels[0].ID
		}
	}

	_ = saveDB(db)
	_ = saveConfig(cfg)
	return cfg.TopModels, workingCount, nil
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

// proxyToRemoteServer forwards Desktop requests to the Ubuntu VPS when Remote Hub Mode is enabled
func proxyToRemoteServer(cfg AppConfig, w http.ResponseWriter, r *http.Request) bool {
	if !cfg.UseRemoteServer || strings.TrimSpace(cfg.RemoteServerURL) == "" {
		return false
	}
	// Do not proxy local connection settings calls
	if r.URL.Path == "/api/local-connection" || r.URL.Path == "/" {
		return false
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.RemoteServerURL), "/")
	targetURL := base + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
	}

	req, err := http.NewRequest(r.Method, targetURL, bytes.NewBuffer(bodyBytes))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": "Invalid Remote Server URL: " + err.Error(),
		})
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.RemoteSecret != "" {
		req.Header.Set("X-Cortext-Secret", cfg.RemoteSecret)
	}

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": "Cannot reach Ubuntu VPS (" + base + "): " + err.Error(),
		})
		return true
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

func buildHTTPMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Auth middleware for when running on Ubuntu VPS with a secret password
	withAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Cortext-Secret")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			if serverSecret != "" && r.URL.Path != "/" {
				got := r.Header.Get("X-Cortext-Secret")
				if got != serverSecret {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"ok":    false,
						"error": "Unauthorized: Invalid Server Secret password.",
					})
					return
				}
			}
			next(w, r)
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(uiHTML))
	})

	// Configure Desktop Local vs Remote Ubuntu VPS Mode
	mux.HandleFunc("/api/local-connection", withAuth(func(w http.ResponseWriter, r *http.Request) {
		dbMutex.Lock()
		defer dbMutex.Unlock()

		var req struct {
			UseRemoteServer bool   `json:"use_remote_server"`
			RemoteServerURL string `json:"remote_server_url"`
			RemoteSecret    string `json:"remote_secret"`
			TestOnly        bool   `json:"test_only"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.TestOnly {
			base := strings.TrimRight(strings.TrimSpace(req.RemoteServerURL), "/")
			testReq, err := http.NewRequest("GET", base+"/api/ping", nil)
			if err != nil {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "message": err.Error()})
				return
			}
			if req.RemoteSecret != "" {
				testReq.Header.Set("X-Cortext-Secret", req.RemoteSecret)
			}
			client := &http.Client{Timeout: 8 * time.Second}
			start := time.Now()
			resp, err := client.Do(testReq)
			lat := time.Since(start).Milliseconds()
			if err != nil {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "message": "Connection failed: " + err.Error()})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "message": "Server rejected Secret Token (401 Unauthorized)"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"message": fmt.Sprintf("Connected to Ubuntu VPS Hub (%d ms ping)!", lat),
			})
			return
		}

		cfg := loadConfig()
		cfg.UseRemoteServer = req.UseRemoteServer
		cfg.RemoteServerURL = strings.TrimSpace(req.RemoteServerURL)
		cfg.RemoteSecret = strings.TrimSpace(req.RemoteSecret)
		_ = saveConfig(cfg)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     true,
			"config": cfg,
		})
	}))

	mux.HandleFunc("/api/ping", withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "server": "sahand-cortext-hub"})
	}))

	mux.HandleFunc("/api/state", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
			return
		}

		dbMutex.Lock()
		defer dbMutex.Unlock()

		db := loadDB()
		rebuildRankedModels(&localCfg, &db)
		_ = saveConfig(localCfg)
		todayStats := getTodayStats(&db)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"config":       localCfg,
			"active_model": localCfg.ActiveModel,
			"top_models":   localCfg.TopModels,
			"sessions":     db.Sessions,
			"today_stats":  todayStats,
		})
	}))

	mux.HandleFunc("/api/select-model", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
			return
		}

		dbMutex.Lock()
		defer dbMutex.Unlock()

		var req struct {
			ModelID       string `json:"model_id"`
			AutoFailover  *bool  `json:"auto_failover"`
			SlidingWindow int    `json:"sliding_window"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		db := loadDB()
		if req.ModelID != "" {
			localCfg.ActiveModel = req.ModelID
		}
		if req.AutoFailover != nil {
			localCfg.AutoFailover = *req.AutoFailover
		}
		if req.SlidingWindow > 0 {
			localCfg.SlidingWindow = req.SlidingWindow
		}
		rebuildRankedModels(&localCfg, &db)
		if req.ModelID != "" {
			localCfg.ActiveModel = req.ModelID
		}
		_ = saveConfig(localCfg)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":           true,
			"config":       localCfg,
			"active_model": localCfg.ActiveModel,
			"top_models":   localCfg.TopModels,
		})
	}))

	// Smart Benchmark & Auto-Rank Top 5 Working Models
	mux.HandleFunc("/api/benchmark-models", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
			return
		}

		var req struct {
			APIKey string `json:"api_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			apiKey = localCfg.GeminiAPIKey
		}
		if apiKey == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"valid":   false,
				"message": "Please enter your Gemini API Key first.",
			})
			return
		}

		topModels, workingCount, _ := runSmartBenchmark(apiKey)
		updatedCfg := loadConfig()
		db := loadDB()

		w.Header().Set("Content-Type", "application/json")
		if workingCount == 0 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"valid":   false,
				"message": "No models could connect. Check your API key or internet/server connection.",
				"config":  updatedCfg,
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":         true,
			"message":       fmt.Sprintf("Smart Scan complete! Found %d working models. Best: %s", workingCount, updatedCfg.ActiveModel),
			"top_models":    topModels,
			"active_model":  updatedCfg.ActiveModel,
			"config":        updatedCfg,
			"today_stats":   getTodayStats(&db),
		})
	}))

	mux.HandleFunc("/api/messages", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
			return
		}

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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"messages": msgs})
	}))

	mux.HandleFunc("/api/session/new", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
			return
		}

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
	}))

	mux.HandleFunc("/api/session/delete", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
			return
		}

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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"sessions": db.Sessions})
	}))

	// Chat endpoint with Continuous Learning + Automatic Top-5 Failover
	mux.HandleFunc("/api/chat", withAuth(func(w http.ResponseWriter, r *http.Request) {
		localCfg := loadConfig()
		if proxyToRemoteServer(localCfg, w, r) {
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

		if localCfg.GeminiAPIKey == "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "API Key is missing. Open Settings and run Smart Scan.",
			})
			return
		}

		dbMutex.Lock()
		db := loadDB()
		history := db.Messages[req.SessionID]
		windowLimit := localCfg.SlidingWindow
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

		// Build ordered candidate list starting with ActiveModel, followed by Top 5 working models for Auto-Failover
		tryOrder := []string{localCfg.ActiveModel}
		if localCfg.AutoFailover {
			for _, tm := range localCfg.TopModels {
				if tm.ID != localCfg.ActiveModel {
					tryOrder = append(tryOrder, tm.ID)
				}
			}
		}

		var (
			reply        string
			pTok, cTok   int
			tTok, totTok int
			latency      int64
			lastErr      error
			usedModel    string
			failoverNote string
		)

		for idx, candidateModel := range tryOrder {
			reply, pTok, cTok, tTok, totTok, latency, lastErr = callGeminiGenerate(
				localCfg.GeminiAPIKey, candidateModel, contents, systemPrompt, 50*time.Second,
			)

			dbMutex.Lock()
			db = loadDB()
			if lastErr == nil {
				recordModelOutcome(&db, candidateModel, true, latency, "")
				usedModel = candidateModel
				if idx > 0 {
					failoverNote = fmt.Sprintf("Auto-switched from %s → %s", localCfg.ActiveModel, candidateModel)
					localCfg.ActiveModel = candidateModel
				}
				rebuildRankedModels(&localCfg, &db)
				_ = saveDB(db)
				_ = saveConfig(localCfg)
				dbMutex.Unlock()
				break
			} else {
				recordModelOutcome(&db, candidateModel, false, latency, lastErr.Error())
				rebuildRankedModels(&localCfg, &db)
				_ = saveDB(db)
				_ = saveConfig(localCfg)
				dbMutex.Unlock()
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if lastErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":         false,
				"error":      fmt.Sprintf("Model error (%s): %v", localCfg.ActiveModel, lastErr),
				"top_models": localCfg.TopModels,
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
			ModelUsed:       usedModel,
			PromptTokens:    pTok,
			CandidateTokens: cTok,
			ThoughtTokens:   tTok,
			TotalTokens:     totTok,
			LatencyMs:       latency,
			FailoverNote:    failoverNote,
			CreatedAt:       now,
		}
		db.NextMessageID++

		db.Messages[req.SessionID] = append(db.Messages[req.SessionID], userMsg, aiMsg)

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
		rebuildRankedModels(&localCfg, &db)
		_ = saveDB(db)
		_ = saveConfig(localCfg)
		dbMutex.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":           true,
			"user_message": userMsg,
			"ai_message":   aiMsg,
			"active_model": localCfg.ActiveModel,
			"top_models":   localCfg.TopModels,
			"sessions":     db.Sessions,
			"today_stats":  todayStats,
		})
	}))

	return mux
}

func main() {
	serverMode := flag.Bool("server", false, "Run as headless Ubuntu VPS Hub Server")
	port := flag.String("port", "8080", "Server port (used in -server mode)")
	secret := flag.String("secret", "", "Optional secret password for VPS Hub authentication")
	apiKeyFlag := flag.String("apikey", "", "Optional Gemini API Key to seed on VPS")
	flag.Parse()

	if envSecret := os.Getenv("CORTEXT_SECRET"); envSecret != "" && *secret == "" {
		*secret = envSecret
	}
	if envPort := os.Getenv("PORT"); envPort != "" && *port == "8080" {
		*port = envPort
	}
	serverSecret = strings.TrimSpace(*secret)

	if *apiKeyFlag != "" {
		cfg := loadConfig()
		cfg.GeminiAPIKey = strings.TrimSpace(*apiKeyFlag)
		cfg.Verified = true
		_ = saveConfig(cfg)
	}

	mux := buildHTTPMux()

	// Headless Ubuntu VPS Mode (or Linux where desktopLauncher is nil)
	if *serverMode || desktopLauncher == nil {
		addr := "0.0.0.0:" + *port
		fmt.Printf("====================================================\n")
		fmt.Printf("🚀 Sahand CorText Hub Server listening on %s\n", addr)
		fmt.Printf("====================================================\n")
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Printf("Server error: %v\n", err)
		}
		return
	}

	// Windows Desktop GUI Mode
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return
	}
	go func() {
		_ = http.Serve(listener, mux)
	}()

	serverURL := fmt.Sprintf("http://%s", listener.Addr().String())
	desktopLauncher(serverURL)
}

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Sahand CorText — Self-Learning AI Hub</title>
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
    --success: #10b981;
    --danger: #ef4444;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; font-family: 'Segoe UI', system-ui, sans-serif; }
  body { background: var(--bg); color: var(--text); display: flex; height: 100vh; overflow: hidden; }

  .sidebar {
    width: 260px; background: #070b14; border-right: 1px solid var(--border);
    display: flex; flex-direction: column; padding: 18px 14px;
  }
  .brand {
    font-size: 17px; font-weight: 700; color: #fff;
    display: flex; align-items: center; justify-content: space-between;
    padding: 4px 6px 14px; border-bottom: 1px solid #1e293b;
  }
  .brand-left { display: flex; align-items: center; gap: 10px; }
  .brand-dot {
    width: 10px; height: 10px; border-radius: 50%;
    background: var(--accent); box-shadow: 0 0 10px var(--accent);
  }
  .hub-badge {
    font-size: 10px; padding: 2px 7px; border-radius: 999px;
    background: #1e293b; color: #38bdf8; border: 1px solid #0284c7;
  }
  .new-chat-btn {
    margin-top: 14px; width: 100%; padding: 10px 12px;
    background: var(--accent); color: #fff; border: none; border-radius: 8px;
    font-weight: 600; font-size: 13px; cursor: pointer;
  }
  .new-chat-btn:hover { background: var(--accent-hover); }
  .section-label {
    font-size: 11px; text-transform: uppercase; letter-spacing: 0.8px;
    color: var(--muted); margin: 16px 6px 8px; font-weight: 600;
  }
  .session-list { flex: 1; overflow-y: auto; display: flex; flex-direction: column; gap: 4px; }
  .session-item {
    display: flex; justify-content: space-between; align-items: center;
    padding: 9px 10px; border-radius: 8px; font-size: 13px; color: #cbd5e1;
    cursor: pointer; border: 1px solid transparent;
  }
  .session-item:hover { background: #111827; }
  .session-item.active { background: #1e293b; border-color: var(--border); color: #fff; font-weight: 600; }
  .session-title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 160px; }
  .session-meta { font-size: 11px; color: var(--muted); display: flex; align-items: center; gap: 6px; }
  .del-btn { background: transparent; border: none; color: var(--muted); cursor: pointer; font-size: 14px; padding: 0 4px; }
  .del-btn:hover { color: var(--danger); }

  .sidebar-footer { border-top: 1px solid #1e293b; padding-top: 12px; display: flex; flex-direction: column; gap: 8px; }
  .nav-btn {
    width: 100%; padding: 10px 12px; background: #111827; border: 1px solid var(--border);
    color: #e2e8f0; border-radius: 8px; font-size: 13px; font-weight: 600;
    cursor: pointer; text-align: left; display: flex; justify-content: space-between; align-items: center;
  }
  .nav-btn:hover { border-color: var(--accent); }

  .main { flex: 1; display: flex; flex-direction: column; height: 100vh; overflow: hidden; }

  /* Top 5 Smart Models Bar */
  .topbar {
    background: #0f172a; border-bottom: 1px solid var(--border);
    padding: 10px 20px; display: flex; flex-direction: column; gap: 8px;
  }
  .topbar-row1 { display: flex; justify-content: space-between; align-items: center; gap: 10px; flex-wrap: wrap; }
  .top5-group {
    display: flex; align-items: center; gap: 6px; flex-wrap: wrap;
  }
  .model-slot-btn {
    padding: 6px 11px; border: 1px solid var(--border); background: #070b14;
    color: #cbd5e1; font-size: 12px; font-weight: 600; border-radius: 8px;
    cursor: pointer; display: flex; align-items: center; gap: 6px; transition: all 0.15s;
  }
  .model-slot-btn:hover { border-color: var(--accent); color: #fff; }
  .model-slot-btn.active {
    background: rgba(16, 185, 129, 0.16); color: #34d399;
    border-color: rgba(16, 185, 129, 0.55); box-shadow: 0 0 12px rgba(16,185,129,0.15);
  }
  .slot-rank {
    font-size: 10px; padding: 1px 5px; border-radius: 4px;
    background: #1e293b; color: #94a3b8; font-family: Consolas, monospace;
  }
  .model-slot-btn.active .slot-rank { background: #065f46; color: #a7f3d0; }
  .slot-stat { font-size: 10px; color: var(--muted); font-family: Consolas, monospace; }

  .ledger { display: flex; gap: 10px; align-items: center; font-size: 12px; }
  .ledger-box {
    background: #1e293b; border: 1px solid var(--border);
    padding: 4px 10px; border-radius: 7px; color: #cbd5e1;
  }
  .ledger-box b { color: #fff; font-family: Consolas, monospace; }

  #chatView { flex: 1; display: flex; flex-direction: column; overflow: hidden; }
  .messages-container { flex: 1; overflow-y: auto; padding: 22px 30px; display: flex; flex-direction: column; gap: 16px; }
  .msg-row { display: flex; flex-direction: column; max-width: 82%; }
  .msg-row.user { align-self: flex-end; align-items: flex-end; }
  .msg-row.model { align-self: flex-start; align-items: flex-start; }
  .msg-bubble {
    padding: 13px 17px; border-radius: 12px; font-size: 14px;
    line-height: 1.6; white-space: pre-wrap; word-break: break-word;
  }
  .msg-row.user .msg-bubble { background: var(--accent); color: #fff; border-bottom-right-radius: 3px; }
  .msg-row.model .msg-bubble { background: var(--card); border: 1px solid var(--border); color: var(--text); border-bottom-left-radius: 3px; }
  .msg-telemetry { margin-top: 5px; font-size: 11px; color: var(--muted); font-family: Consolas, monospace; }

  .composer {
    padding: 14px 26px 18px; background: #0f172a; border-top: 1px solid var(--border);
    display: flex; gap: 10px; align-items: flex-end;
  }
  .composer textarea {
    flex: 1; background: #070b14; border: 1px solid var(--border); border-radius: 10px;
    color: #fff; padding: 12px 14px; font-size: 14px; resize: none; height: 52px; outline: none;
  }
  .composer textarea:focus { border-color: var(--accent); }
  .send-btn {
    height: 52px; padding: 0 24px; background: var(--accent); color: #fff;
    border: none; border-radius: 10px; font-weight: 700; font-size: 14px; cursor: pointer;
  }
  .send-btn:disabled { opacity: 0.5; cursor: not-allowed; }

  /* Settings View */
  #settingsView { flex: 1; overflow-y: auto; padding: 28px; display: none; justify-content: center; align-items: flex-start; gap: 22px; flex-wrap: wrap; }
  .settings-card {
    width: 100%; max-width: 520px; background: var(--card);
    border: 1px solid var(--border); border-radius: 12px; padding: 24px;
  }
  .settings-card h2 { font-size: 17px; margin-bottom: 6px; }
  .settings-card p { font-size: 12px; color: var(--muted); margin-bottom: 16px; line-height: 1.5; }
  label { display: block; font-size: 12px; font-weight: 600; color: #cbd5e1; margin-bottom: 6px; margin-top: 12px; }
  input, select {
    width: 100%; padding: 9px 12px; background: #0b1120;
    border: 1px solid var(--border); border-radius: 8px; color: #fff; font-size: 13px;
  }
  .row-flex { display: flex; gap: 8px; align-items: center; }
  .btn-primary {
    margin-top: 16px; width: 100%; padding: 11px; background: var(--accent);
    color: #fff; border: none; border-radius: 8px; font-weight: 600; cursor: pointer;
  }
  .btn-secondary {
    margin-top: 10px; width: 100%; padding: 9px; background: #111827;
    color: #cbd5e1; border: 1px solid var(--border); border-radius: 8px; font-weight: 600; cursor: pointer;
  }
  .leaderboard {
    margin-top: 14px; background: #0b1120; border: 1px solid var(--border);
    border-radius: 8px; padding: 10px; font-size: 11px; font-family: Consolas, monospace;
    max-height: 190px; overflow-y: auto; line-height: 1.7;
  }
</style>
</head>
<body>
  <div class="sidebar">
    <div>
      <div class="brand">
        <div class="brand-left"><span class="brand-dot"></span> sahand-cortext</div>
        <span id="hubBadge" class="hub-badge">Local</span>
      </div>
      <button class="new-chat-btn" onclick="createNewSession()">+ New Chat Session</button>
      <div class="section-label">Conversations</div>
    </div>

    <div id="sessionList" class="session-list"></div>

    <div class="sidebar-footer">
      <button class="nav-btn" onclick="toggleView('chat')"><span>💬 Chat Workspace</span></button>
      <button class="nav-btn" onclick="toggleView('settings')">
        <span>⚙️ Smart Models & VPS Hub</span>
        <span id="keyStatusDot" style="color: #10b981;">●</span>
      </button>
    </div>
  </div>

  <div class="main">
    <div class="topbar">
      <div class="topbar-row1">
        <div id="top5Bar" class="top5-group"></div>
        <div class="ledger">
          <div class="ledger-box">Session: <b id="sessionTokens">0</b> tok</div>
          <div class="ledger-box">Today: <b id="todayReqs">0</b> reqs • <b id="todayTokens">0</b> tok</div>
        </div>
      </div>
    </div>

    <div id="chatView">
      <div id="messagesBox" class="messages-container"></div>
      <div class="composer">
        <textarea id="chatInput" placeholder="Message Sahand CorText... (Auto-Failover across Top 5 Working Models is active)"></textarea>
        <button id="sendBtn" class="send-btn" onclick="sendMessage()">Send</button>
      </div>
    </div>

    <div id="settingsView">
      <!-- CARD 1: SMART SELF-LEARNING MODEL ENGINE -->
      <div class="settings-card">
        <h2>⚡ Smart Top-5 Model Engine</h2>
        <p>Tests candidate models with live probes, prioritizes higher versions (e.g. <b>3.8 Lite > 3.5 Lite</b>), and continuously learns from latency and success rates.</p>

        <label>Gemini API Key</label>
        <div class="row-flex">
          <input type="password" id="apiKeyInput" placeholder="AIzaSy..." />
          <button class="nav-btn" style="width:auto;" onclick="togglePassword()">Show</button>
        </div>

        <label style="display:flex; align-items:center; gap:8px; margin-top:14px; cursor:pointer;">
          <input type="checkbox" id="autoFailoverCheck" style="width:auto;" onchange="saveModelPrefs()" checked />
          <span>Automatically switch to next Top-5 working model if current model fails</span>
        </label>

        <label>Manual Override (All Discovered Models)</label>
        <select id="allModelsSelect" onchange="selectSpecificModel(this.value)"></select>

        <button id="scanBtn" class="btn-primary" onclick="runSmartScan()">⚡ Smart Scan & Benchmark Top 5 Working Models</button>
        <div id="scanStatus" style="margin-top:10px; font-size:12px;"></div>

        <div class="LeaderboardTitle" style="font-size:11px; color:#94a3b8; margin-top:12px; font-weight:600;">LIVE SELF-LEARNING LEADERBOARD (TOP 5):</div>
        <div id="leaderboardBox" class="leaderboard">Run Smart Scan to benchmark models...</div>
      </div>

      <!-- CARD 2: UBUNTU VPS SERVER HUB -->
      <div class="settings-card">
        <h2>☁️ Ubuntu VPS Server Hub (Ireland / US)</h2>
        <p>Route all AI requests, IP identity, and database storage through your Ubuntu VPS so Desktop, Telegram, and Mobile share one brain and one server IP.</p>

        <label style="display:flex; align-items:center; gap:8px; cursor:pointer;">
          <input type="checkbox" id="useRemoteCheck" style="width:auto;" />
          <span><b>Enable Remote Ubuntu VPS Hub Mode</b></span>
        </label>

        <label>Ubuntu Server URL (IP & Port or Domain)</label>
        <input type="text" id="remoteUrlInput" placeholder="http://YOUR_SERVER_IP:8080" />

        <label>Server Secret Password (Optional)</label>
        <input type="password" id="remoteSecretInput" placeholder="Secret token set with -secret flag" />

        <button class="btn-secondary" onclick="testRemoteServer()">🔌 Test Connection to Ubuntu Server</button>
        <button class="btn-primary" onclick="saveRemoteServer()">💾 Save Connection Mode</button>
        <div id="remoteStatus" style="margin-top:10px; font-size:12px;"></div>
      </div>
    </div>
  </div>

<script>
  let appState = {
    config: {},
    activeModel: '',
    topModels: [],
    sessions: [],
    currentSessionId: 1,
    todayStats: {}
  };

  window.addEventListener('DOMContentLoaded', async () => {
    document.getElementById('chatInput').addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        sendMessage();
      }
    });
    await loadAppState();
  });

  async function loadAppState() {
    try {
      const res = await fetch('/api/state');
      const data = await res.json();
      if (data.error) {
        toggleView('settings');
        document.getElementById('remoteStatus').style.color = '#ef4444';
        document.getElementById('remoteStatus').textContent = '✖ ' + data.error;
        return;
      }
      appState.config = data.config || {};
      appState.activeModel = data.active_model || '';
      appState.topModels = data.top_models || [];
      appState.sessions = data.sessions || [];
      appState.todayStats = data.today_stats || {};

      if (appState.sessions.length > 0 && !appState.sessions.some(s => s.id === appState.currentSessionId)) {
        appState.currentSessionId = appState.sessions[0].id;
      }

      syncUI();

      if (!appState.config.verified && !appState.config.use_remote_server) {
        toggleView('settings');
      } else {
        toggleView('chat');
        await loadMessages(appState.currentSessionId);
      }
    } catch (e) {
      toggleView('settings');
    }
  }

  function syncUI() {
    const cfg = appState.config || {};
    const badge = document.getElementById('hubBadge');
    if (cfg.use_remote_server) {
      badge.textContent = '☁️ VPS Hub';
      badge.style.color = '#34d399';
      badge.style.borderColor = '#10b981';
    } else {
      badge.textContent = '🖥️ Local';
      badge.style.color = '#38bdf8';
      badge.style.borderColor = '#0284c7';
    }

    // Render Top 5 Working Models Bar
    const bar = document.getElementById('top5Bar');
    bar.innerHTML = '';
    const top5 = appState.topModels || [];
    top5.forEach((m, idx) => {
      const btn = document.createElement('button');
      const isAct = (m.id === appState.activeModel);
      btn.className = 'model-slot-btn' + (isAct ? ' active' : '');
      btn.onclick = () => selectSpecificModel(m.id);

      const total = (m.success_count || 0) + (m.fail_count || 0);
      const rate = total > 0 ? Math.round((m.success_count / total) * 100) + '%' : 'untested';
      const lat = m.avg_latency_ms ? (m.avg_latency_ms + 'ms') : '';
      const statStr = lat ? (lat + ' • ' + rate) : rate;

      btn.innerHTML = '<span class="slot-rank">#' + (idx + 1) + '</span>' +
        '<span>' + m.id + '</span>' +
        '<span class="slot-stat">' + statStr + '</span>';
      bar.appendChild(btn);
    });

    // Leaderboard Box in Settings
    const lb = document.getElementById('leaderboardBox');
    if (top5.length > 0) {
      lb.innerHTML = top5.map((m, i) => {
        const tot = (m.success_count || 0) + (m.fail_count || 0);
        const pct = tot > 0 ? Math.round((m.success_count / tot) * 100) : 0;
        return '#' + (i + 1) + ' <b>' + m.id + '</b> (v' + m.version + ') — Score: ' + m.smart_score +
          ' | OK: ' + m.success_count + '/' + tot + ' (' + pct + '%) | Avg: ' + (m.avg_latency_ms || '--') + 'ms';
      }).join('<br>');
    }

    // Populate All Models Dropdown
    const sel = document.getElementById('allModelsSelect');
    sel.innerHTML = '';
    (cfg.available_models || []).forEach(m => {
      const opt = document.createElement('option');
      opt.value = m.id;
      const status = m.verified_working ? '✔ ' : (m.fail_count > 0 ? '✖ ' : '');
      opt.textContent = status + m.id + ' (Score: ' + m.smart_score + ')';
      sel.appendChild(opt);
    });
    if (appState.activeModel) sel.value = appState.activeModel;

    // Stats
    const st = appState.todayStats || {};
    document.getElementById('todayReqs').textContent = (st.requests_count || 0).toLocaleString();
    document.getElementById('todayTokens').textContent = (st.total_tokens || 0).toLocaleString();
    const currSess = (appState.sessions || []).find(s => s.id === appState.currentSessionId);
    document.getElementById('sessionTokens').textContent = currSess ? (currSess.total_tokens || 0).toLocaleString() : '0';

    // Sidebar Sessions
    const list = document.getElementById('sessionList');
    list.innerHTML = '';
    (appState.sessions || []).forEach(s => {
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
      delBtn.onclick = (ev) => { ev.stopPropagation(); deleteSession(s.id); };

      meta.appendChild(tokSpan);
      meta.appendChild(delBtn);
      div.appendChild(titleSpan);
      div.appendChild(meta);
      list.appendChild(div);
    });

    // Settings Inputs
    document.getElementById('apiKeyInput').value = cfg.gemini_api_key || '';
    document.getElementById('autoFailoverCheck').checked = cfg.auto_failover !== false;
    document.getElementById('useRemoteCheck').checked = !!cfg.use_remote_server;
    document.getElementById('remoteUrlInput').value = cfg.remote_server_url || '';
    document.getElementById('remoteSecretInput').value = cfg.remote_secret || '';
  }

  function toggleView(view) {
    document.getElementById('chatView').style.display = (view === 'chat') ? 'flex' : 'none';
    document.getElementById('settingsView').style.display = (view === 'settings') ? 'flex' : 'none';
  }

  async function selectSpecificModel(modelId) {
    const res = await fetch('/api/select-model', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model_id: modelId })
    });
    const data = await res.json();
    if (data.ok) {
      appState.config = data.config;
      appState.activeModel = data.active_model;
      appState.topModels = data.top_models;
      syncUI();
    }
  }

  async function saveModelPrefs() {
    const failover = document.getElementById('autoFailoverCheck').checked;
    const res = await fetch('/api/select-model', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ auto_failover: failover })
    });
    const data = await res.json();
    if (data.ok) {
      appState.config = data.config;
      syncUI();
    }
  }

  async function runSmartScan() {
    const apiKey = document.getElementById('apiKeyInput').value.trim();
    const btn = document.getElementById('scanBtn');
    const status = document.getElementById('scanStatus');

    btn.disabled = true;
    btn.textContent = 'Probing Models in Parallel (takes ~5s)...';
    status.style.color = '#94a3b8';
    status.textContent = 'Testing live connectivity & ranking highest versions first...';

    try {
      const res = await fetch('/api/benchmark-models', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ api_key: apiKey })
      });
      const data = await res.json();
      if (data.valid) {
        appState.config = data.config;
        appState.activeModel = data.active_model;
        appState.topModels = data.top_models;
        appState.todayStats = data.today_stats;
        syncUI();
        status.style.color = '#10b981';
        status.textContent = '✔ ' + data.message;
      } else {
        status.style.color = '#ef4444';
        status.textContent = '✖ ' + (data.message || data.error);
      }
    } catch (e) {
      status.style.color = '#ef4444';
      status.textContent = '✖ Error: ' + e.message;
    } finally {
      btn.disabled = false;
      btn.textContent = '⚡ Smart Scan & Benchmark Top 5 Working Models';
    }
  }

  async function testRemoteServer() {
    const url = document.getElementById('remoteUrlInput').value.trim();
    const secret = document.getElementById('remoteSecretInput').value.trim();
    const st = document.getElementById('remoteStatus');
    st.style.color = '#94a3b8';
    st.textContent = 'Pinging Ubuntu Server...';

    const res = await fetch('/api/local-connection', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ remote_server_url: url, remote_secret: secret, test_only: true })
    });
    const data = await res.json();
    st.style.color = data.ok ? '#10b981' : '#ef4444';
    st.textContent = (data.ok ? '✔ ' : '✖ ') + data.message;
  }

  async function saveRemoteServer() {
    const useRemote = document.getElementById('useRemoteCheck').checked;
    const url = document.getElementById('remoteUrlInput').value.trim();
    const secret = document.getElementById('remoteSecretInput').value.trim();
    const st = document.getElementById('remoteStatus');

    const res = await fetch('/api/local-connection', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        use_remote_server: useRemote,
        remote_server_url: url,
        remote_secret: secret,
        test_only: false
      })
    });
    const data = await res.json();
    if (data.ok) {
      st.style.color = '#10b981';
      st.textContent = '✔ Connection mode saved! Reloading state...';
      await loadAppState();
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
        'Active Brain: <b style="color:#34d399">' + appState.activeModel + '</b><br>' +
        'Click any of your Top 5 Working Models in the top bar, or just start typing.</div>';
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
      let note = m.failover_note ? (' [' + m.failover_note + '] • ') : '';
      meta.textContent = note + m.model_used + ' • ' + m.latency_ms + 'ms • ' +
        m.total_tokens + ' tok (in:' + m.prompt_tokens + ' / out:' + m.candidate_tokens + ')';
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

    const box = document.getElementById('messagesBox');
    if (box.children.length === 1 && !box.children[0].classList.contains('msg-row')) {
      box.innerHTML = '';
    }
    appendMessageElement({ role: 'user', content: text });

    try {
      const res = await fetch('/api/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: appState.currentSessionId, text: text })
      });
      const data = await res.json();
      if (!data.ok) {
        if (data.top_models) appState.topModels = data.top_models;
        syncUI();
        appendMessageElement({
          role: 'model', content: '⚠️ ' + data.error,
          model_used: appState.activeModel, latency_ms: 0, total_tokens: 0, prompt_tokens: 0, candidate_tokens: 0
        });
      } else {
        appState.activeModel = data.active_model;
        appState.topModels = data.top_models;
        appState.sessions = data.sessions;
        appState.todayStats = data.today_stats;
        syncUI();
        appendMessageElement(data.ai_message);
      }
    } catch (err) {
      appendMessageElement({
        role: 'model', content: '⚠️ Connection error: ' + err.message,
        model_used: 'system', latency_ms: 0, total_tokens: 0, prompt_tokens: 0, candidate_tokens: 0
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
</script>
</body>
</html>
`
