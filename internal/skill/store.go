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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SourceURL   string `json:"source_url"`
	StorageMode string `json:"storage_mode"` // "full" | "compressed"
	Content     string `json:"content"`
	PersonaID   string `json:"persona_id"` // "" = global (all bots & main chat), or specific persona ID
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
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
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS skills (
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
	);`)
	return err
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
func (s *Store) BuildSkillPromptBlock(ctx context.Context, personaID, userMessage string) string {
	skills, err := s.List(ctx, personaID, true)
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
