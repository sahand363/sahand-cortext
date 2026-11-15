package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

var ErrSkillNotFound = errors.New("skill not found")

type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	SourceURL   string   `json:"source_url"`
	StorageMode string   `json:"storage_mode"` // "full" | "compressed"
	Content     string   `json:"content"`
	PersonaID   string   `json:"persona_id"` // "" = global (all bots & main chat), or specific persona ID
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Scopes      []string `json:"scopes,omitempty"` // assignment scopes: "all", "main", or persona ids
}

type StagedDraft struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	SourceURL         string `json:"source_url"`
	FullContent       string `json:"full_content"`
	CompressedContent string `json:"compressed_content"`
	PersonaID         string `json:"persona_id"`
	CreatedAt         time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) InitSchema() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS skills (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		source_url TEXT NOT NULL DEFAULT '',
		storage_mode TEXT NOT NULL DEFAULT 'compressed',
		content TEXT NOT NULL,
		persona_id TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`); err != nil {
		return err
	}
	// One skill can belong to many places: the main chat ("main"), any bot
	// (its persona id), or everyone ("all"). The assignments table is the
	// source of truth for visibility; persona_id stays as the "learned by" label.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_assignments (
		skill_id TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		PRIMARY KEY(skill_id, scope_id)
	);`); err != nil {
		return err
	}
	// Backfill: a skill learned by a bot belongs to that bot; a global skill
	// (persona_id = '') belongs to everyone.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO skill_assignments (skill_id, scope_id)
		SELECT id, CASE WHEN persona_id = '' THEN 'all' ELSE persona_id END FROM skills`); err != nil {
		return err
	}
	return nil
}

// ScopeAll is the assignment scope meaning "visible to everyone" (main chat + all bots).
const ScopeAll = "all"

// ScopeMain is the assignment scope for the main (web panel) chat.
const ScopeMain = "main"

// SetScopes replaces the assignment scopes of a skill (deduped, trimmed).
// Pass ScopeAll ("all") to make the skill visible to everyone.
func (s *Store) SetScopes(ctx context.Context, skillID string, scopes []string) error {
	id := CanonicalID(skillID)
	if id == "" {
		return errors.New("skill id is required")
	}
	seen := map[string]bool{}
	var clean []string
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if sc == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		clean = append(clean, sc)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM skill_assignments WHERE skill_id = ?`, id); err != nil {
		return err
	}
	for _, sc := range clean {
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO skill_assignments (skill_id, scope_id) VALUES (?, ?)`, id, sc); err != nil {
			return err
		}
	}
	return nil
}

// ListVisible returns the enabled skills visible to a caller that holds the
// given scopes (e.g. ["main"] for the main chat, or a bot's persona id).
// A skill assigned to ScopeAll ("all") is visible to every caller, and a
// legacy skill with persona_id = '' (global) is visible too, so skills saved
// before assignments existed keep working. The rows are fully read and closed
// before returning, so the caller may safely run the long AI call afterwards
// on the single-connection SQLite.
func (s *Store) ListVisible(ctx context.Context, scopes []string) ([]Skill, error) {
	seen := map[string]bool{ScopeAll: true}
	var args []any
	args = append(args, ScopeAll)
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if sc == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		args = append(args, sc)
	}
	placeholders := strings.Repeat("?,", len(args))
	placeholders = strings.TrimSuffix(placeholders, ",")
	q := `SELECT s.id, s.name, s.description, s.source_url, s.storage_mode, s.content, s.persona_id, s.enabled, s.created_at, s.updated_at
		FROM skills s
		WHERE s.enabled = 1 AND (s.persona_id = '' OR EXISTS (
			SELECT 1 FROM skill_assignments a WHERE a.skill_id = s.id AND a.scope_id IN (` + placeholders + `)
		))
		ORDER BY s.updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		var sk Skill
		var en int
		if rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.SourceURL, &sk.StorageMode, &sk.Content, &sk.PersonaID, &en, &sk.CreatedAt, &sk.UpdatedAt) == nil {
			sk.Enabled = en != 0
			out = append(out, sk)
		}
	}
	return out, nil
}

func CanonicalID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Store) Save(ctx context.Context, sk Skill) (*Skill, error) {
	sk.Name = strings.TrimSpace(sk.Name)
	if sk.Name == "" {
		return nil, errors.New("skill name is required")
	}
	if strings.TrimSpace(sk.ID) == "" {
		sk.ID = CanonicalID(sk.Name)
	} else {
		sk.ID = CanonicalID(sk.ID)
	}
	if sk.ID == "" {
		sk.ID = fmt.Sprintf("skill-%d", time.Now().Unix())
	}
	if sk.StorageMode != "full" {
		sk.StorageMode = "compressed"
	}
	sk.Content = strings.TrimSpace(sk.Content)
	if sk.Content == "" {
		return nil, errors.New("skill content cannot be empty")
	}
	enInt := 1
	if !sk.Enabled {
		enInt = 0
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO skills (id, name, description, source_url, storage_mode, content, persona_id, enabled, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			source_url = excluded.source_url,
			storage_mode = excluded.storage_mode,
			content = excluded.content,
			persona_id = excluded.persona_id,
			enabled = excluded.enabled,
			updated_at = CURRENT_TIMESTAMP
	`, sk.ID, sk.Name, strings.TrimSpace(sk.Description), strings.TrimSpace(sk.SourceURL), sk.StorageMode, sk.Content, strings.TrimSpace(sk.PersonaID), enInt)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, sk.ID)
}

func (s *Store) Get(ctx context.Context, id string) (*Skill, error) {
	var sk Skill
	var en int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, description, source_url, storage_mode, content, persona_id, enabled, created_at, updated_at
		FROM skills WHERE id = ?
	`, CanonicalID(id)).Scan(&sk.ID, &sk.Name, &sk.Description, &sk.SourceURL, &sk.StorageMode, &sk.Content, &sk.PersonaID, &en, &sk.CreatedAt, &sk.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSkillNotFound
	}
	if err != nil {
		return nil, err
	}
	sk.Enabled = en != 0
	return &sk, nil
}

func (s *Store) List(ctx context.Context, personaID string, onlyEnabled bool) ([]Skill, error) {
	var rows *sql.Rows
	var err error
	if personaID != "" {
		q := `SELECT id, name, description, source_url, storage_mode, content, persona_id, enabled, created_at, updated_at
		      FROM skills WHERE (persona_id = '' OR persona_id = ?)`
		if onlyEnabled {
			q += ` AND enabled = 1`
		}
		q += ` ORDER BY updated_at DESC`
		rows, err = s.db.QueryContext(ctx, q, personaID)
	} else {
		q := `SELECT id, name, description, source_url, storage_mode, content, persona_id, enabled, created_at, updated_at FROM skills`
		if onlyEnabled {
			q += ` WHERE enabled = 1`
		}
		q += ` ORDER BY updated_at DESC`
		rows, err = s.db.QueryContext(ctx, q)
	}
	if err != nil {
		return nil, err
	}

	var out []Skill
	for rows.Next() {
		var sk Skill
		var en int
		if rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.SourceURL, &sk.StorageMode, &sk.Content, &sk.PersonaID, &en, &sk.CreatedAt, &sk.UpdatedAt) == nil {
			sk.Enabled = en != 0
			out = append(out, sk)
		}
	}
	rows.Close() // release the single SQLite connection BEFORE the scopes query below

	// Attach assignment scopes (one extra query). Skills without assignment
	// rows fall back to their stored persona (or "all" when global).
	if len(out) > 0 {
		if arows, aerr := s.db.QueryContext(ctx, `SELECT skill_id, scope_id FROM skill_assignments`); aerr == nil {
			byID := map[string][]string{}
			for arows.Next() {
				var sid, sc string
				if arows.Scan(&sid, &sc) == nil {
					byID[sid] = append(byID[sid], sc)
				}
			}
			arows.Close()
			for i := range out {
				if scs, ok := byID[out[i].ID]; ok {
					out[i].Scopes = scs
				} else if out[i].PersonaID == "" {
					out[i].Scopes = []string{ScopeAll}
				} else {
					out[i].Scopes = []string{out[i].PersonaID}
				}
			}
		}
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, CanonicalID(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSkillNotFound
	}
	return nil
}

// BuildSkillPromptBlock formats active skills for injection into the LLM system prompt.
// Relevant or small skill sets are injected in full/compressed form; larger catalogs include an index + matched bodies.
// scopes are the caller's assignment scopes (e.g. ["main"] for the main chat,
// or a bot's persona id); only skills visible to those scopes are injected.
func (s *Store) BuildSkillPromptBlock(ctx context.Context, scopes []string, userMessage string) string {
	skills, err := s.ListVisible(ctx, scopes)
	if err != nil || len(skills) == 0 {
		return ""
	}

	userLower := strings.ToLower(userMessage)
	var sb strings.Builder
	sb.WriteString("\n\n=== LEARNED KNOWLEDGE BASE & SKILLS ===\n")
	sb.WriteString("You have learned and stored the following verified skills in your database. Apply them whenever relevant:\n\n")

	totalInjectedChars := 0
	for _, sk := range skills {
		isRelevant := len(skills) <= 4 ||
			strings.Contains(userLower, strings.ToLower(sk.Name)) ||
			hasKeywordOverlap(userLower, sk.Name+" "+sk.Description)

		if isRelevant && totalInjectedChars+len(sk.Content) <= 16000 {
			fmt.Fprintf(&sb, "## Skill: %s (%s)\n", sk.Name, sk.StorageMode)
			if sk.Description != "" {
				fmt.Fprintf(&sb, "Summary: %s\n", sk.Description)
			}
			sb.WriteString(sk.Content)
			sb.WriteString("\n\n")
			totalInjectedChars += len(sk.Content)
		} else {
			fmt.Fprintf(&sb, "- **%s**: %s\n", sk.Name, sk.Description)
		}
	}
	return sb.String()
}

func hasKeywordOverlap(queryLower, meta string) bool {
	words := strings.Fields(strings.ToLower(meta))
	for _, w := range words {
		clean := strings.Trim(w, ".,:;!?()[]{}\"'`")
		if len(clean) >= 4 && strings.Contains(queryLower, clean) {
			return true
		}
	}
	return false
}

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|footer|header)[^>]*>.*?</(script|style|noscript|svg|nav|footer|header)>`)
	tagRe         = regexp.MustCompile(`(?s)<[^>]+>`)
	multiSpaceRe  = regexp.MustCompile(`[ \t]{2,}`)
	multiLineRe   = regexp.MustCompile(`\n{3,}`)
)

// FetchAndCleanURL visits a webpage directly from the Go server and extracts clean readable text.
func FetchAndCleanURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("please provide a valid http:// or https:// URL")
	}

	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sahand-CorText/1.0)")
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to visit link: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("website returned HTTP %d", resp.StatusCode)
	}

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2 MB cap
	if err != nil {
		return "", err
	}

	text := string(rawBytes)
	text = scriptStyleRe.ReplaceAllString(text, "\n")
	text = strings.ReplaceAll(text, "</p>", "\n\n")
	text = strings.ReplaceAll(text, "<br>", "\n")
	text = strings.ReplaceAll(text, "<br/>", "\n")
	text = strings.ReplaceAll(text, "<br />", "\n")
	text = strings.ReplaceAll(text, "</li>", "\n")
	text = strings.ReplaceAll(text, "</h1>", "\n\n")
	text = strings.ReplaceAll(text, "</h2>", "\n\n")
	text = strings.ReplaceAll(text, "</h3>", "\n\n")
	text = tagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = multiSpaceRe.ReplaceAllString(text, " ")
	text = multiLineRe.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)

	if len([]rune(text)) < 40 {
		return "", errors.New("page returned empty or JavaScript-only content that could not be extracted")
	}
	runes := []rune(text)
	if len(runes) > 32000 {
		text = string(runes[:32000]) + "\n\n[Truncated]"
	}
	return text, nil
}

// ExtractSkillFromURL visits the link via Go, then uses the active LLM to synthesize
// both a Full version and a Compressed (redundant-free) version for user confirmation.
func ExtractSkillFromURL(
	ctx context.Context,
	router *provider.Router,
	provID, apiKey, modelID, targetURL, personaID string,
) (*StagedDraft, error) {
	pageText, err := FetchAndCleanURL(ctx, targetURL)
	if err != nil {
		return nil, err
	}

	sysPrompt := `You are a technical knowledge engineer. You have been given the extracted text of a webpage.
Your job is to turn this webpage into a reusable AI Skill with TWO versions:
1. "full_content": Comprehensive reference preserving all technical rules, code examples, edge cases, CLI flags, and workflows from the page.
2. "compressed_content": Ultra-dense, high-signal version. Emit/strip all introductory fluff, marketing copy, repetition, and verbose filler — keeping ONLY hard rules, exact commands, schemas, and key decision logic.

Output ONLY valid JSON matching this exact schema (no markdown fences):
{
  "name": "Short descriptive skill title",
  "description": "1-2 sentence summary of what this skill teaches and when to apply it.",
  "full_content": "Complete markdown instructions and examples...",
  "compressed_content": "Dense, stripped-down markdown rules and commands..."
}`

	userPrompt := fmt.Sprintf("Source URL: %s\n\nExtracted Webpage Content:\n%s", targetURL, pageText)

	rawReply, _, err := router.Call(ctx, provID, apiKey, modelID, []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}, provider.CallOpts{MaxTokens: 4096})
	if err != nil {
		return nil, fmt.Errorf("AI extraction failed: %w", err)
	}

	cleaned := stripJSONFences(rawReply)
	var parsed struct {
		Name              string `json:"name"`
		Description       string `json:"description"`
		FullContent       string `json:"full_content"`
		CompressedContent string `json:"compressed_content"`
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil || strings.TrimSpace(parsed.FullContent) == "" {
		// Fallback if model returned plain markdown instead of strict JSON
		return &StagedDraft{
			Name:              "Learned Skill (" + targetURL + ")",
			Description:       "Extracted knowledge from " + targetURL,
			SourceURL:         targetURL,
			FullContent:       strings.TrimSpace(rawReply),
			CompressedContent: truncateStr(strings.TrimSpace(rawReply), 1800),
			PersonaID:         personaID,
			CreatedAt:         time.Now(),
		}, nil
	}

	if strings.TrimSpace(parsed.CompressedContent) == "" {
		parsed.CompressedContent = truncateStr(parsed.FullContent, 1800)
	}

	return &StagedDraft{
		Name:              strings.TrimSpace(parsed.Name),
		Description:       strings.TrimSpace(parsed.Description),
		SourceURL:         targetURL,
		FullContent:       strings.TrimSpace(parsed.FullContent),
		CompressedContent: strings.TrimSpace(parsed.CompressedContent),
		PersonaID:         personaID,
		CreatedAt:         time.Now(),
	}, nil
}

func stripJSONFences(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		if nl := strings.Index(t, "\n"); nl != -1 {
			t = t[nl+1:]
		}
		if end := strings.LastIndex(t, "```"); end != -1 {
			t = t[:end]
		}
	}
	return strings.TrimSpace(t)
}

func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

var hrefRe = regexp.MustCompile(`(?i)<a[^>]+href\s*=\s*["']([^"']+)["']`)

// ExtractPageLinks fetches a page and returns its same-domain links, in order,
// de-duplicated and capped at maxLinks. Only http(s) links on the same host are kept.
func ExtractPageLinks(ctx context.Context, rawURL string, maxLinks int) ([]string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("please provide a valid http:// or https:// URL")
	}

	client := &http.Client{Timeout: 25 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sahand-CorText/1.0)")
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to visit link: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("website returned HTTP %d", resp.StatusCode)
	}

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2 MB cap
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var links []string
	for _, m := range hrefRe.FindAllStringSubmatch(string(rawBytes), -1) {
		if len(links) >= maxLinks {
			break
		}
		href := strings.TrimSpace(m[1])
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") ||
			strings.HasPrefix(href, "tel:") || strings.HasPrefix(href, "javascript:") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		abs := u.ResolveReference(ref)
		if abs.Host != u.Host || (abs.Scheme != "http" && abs.Scheme != "https") {
			continue
		}
		abs.Fragment = ""
		key := abs.String()
		if key == u.String() || seen[key] {
			continue
		}
		seen[key] = true
		links = append(links, key)
	}
	return links, nil
}

// LearnDeepResult is the outcome of a deep-learn run.
type LearnDeepResult struct {
	Learned []string // names of skills saved
	Failed  []string // URLs that could not be learned
}

// LearnDeep learns a page and the links inside it (same domain only).
// The page itself is learned first, then up to maxPages-1 linked pages.
// Each page is saved as a compressed skill (dense, token-efficient) and enabled.
// onProgress, if non-nil, is called after each page with the running count,
// the page URL, and the saved skill name ("" when the page failed).
// It never holds a DB row open across the AI call: extraction fetches the page
// and calls the model first, and Save is a single short statement afterwards.
func (s *Store) LearnDeep(ctx context.Context, router *provider.Router, provID, apiKey, modelID, targetURL, personaID string, maxPages int, onProgress func(done int, pageURL, skillName string)) LearnDeepResult {
	if maxPages < 1 {
		maxPages = 1
	}
	if maxPages > 10 {
		maxPages = 10
	}
	// A page learned in the main chat (no persona) belongs to the main chat,
	// not to everyone; a page learned by a bot belongs to that bot.
	ownerID := personaID
	if ownerID == "" {
		ownerID = ScopeMain
	}
	var res LearnDeepResult

	learnOne := func(pageURL string) {
		draft, err := ExtractSkillFromURL(ctx, router, provID, apiKey, modelID, pageURL, personaID)
		if err != nil {
			res.Failed = append(res.Failed, pageURL)
			if onProgress != nil {
				onProgress(len(res.Learned)+len(res.Failed), pageURL, "")
			}
			return
		}
		content := strings.TrimSpace(draft.CompressedContent)
		if content == "" {
			content = strings.TrimSpace(draft.FullContent)
		}
		saved, err := s.Save(ctx, Skill{
			Name:        draft.Name,
			Description: draft.Description,
			SourceURL:   draft.SourceURL,
			StorageMode: "compressed",
			Content:     content,
			PersonaID:   ownerID,
			Enabled:     true,
		})
		if err != nil {
			res.Failed = append(res.Failed, pageURL)
		} else {
			_ = s.SetScopes(ctx, saved.ID, []string{ownerID})
			res.Learned = append(res.Learned, saved.Name)
		}
		if onProgress != nil {
			onProgress(len(res.Learned)+len(res.Failed), pageURL, draft.Name)
		}
	}

	learnOne(targetURL)
	if len(res.Learned)+len(res.Failed) >= maxPages {
		return res
	}
	links, err := ExtractPageLinks(ctx, targetURL, maxPages-1)
	if err != nil {
		return res
	}
	for _, link := range links {
		if len(res.Learned)+len(res.Failed) >= maxPages {
			break
		}
		if err := ctx.Err(); err != nil {
			break
		}
		learnOne(link)
	}
	return res
}
