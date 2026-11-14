package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

// TrainRequest describes raw chat logs and target speaker details for distilling a Voice Blueprint.
type TrainRequest struct {
	TargetName  string `json:"target_name"`  // e.g. "Sahand" (the person whose voice the bot should learn)
	RawChatLog  string `json:"raw_chat_log"` // Telegram Desktop JSON export, WhatsApp .txt export, or plain transcript
	ExtraNotes  string `json:"extra_notes"`  // Optional hints (e.g. "Use casual Tehran Persian + English tech terms")
	Provider    string `json:"provider"`     // LLM provider used to distill the voice blueprint
	Model       string `json:"model"`        // LLM model used to distill the voice blueprint
	MaxExamples int    `json:"max_examples"` // Number of few-shot anchor pairs to extract (default 6)
}

// TrainResult holds the distilled Voice Blueprint, extracted few-shot pairs, and sample stats.
type TrainResult struct {
	VoiceBlueprint  string           `json:"voice_blueprint"`
	FewShotExamples []FewShotExample `json:"few_shot_examples"`
	ParsedTurns     int              `json:"parsed_turns"`
	MatchedReplies  int              `json:"matched_replies"`
	DetectedFormat  string           `json:"detected_format"`
}

type chatTurn struct {
	Sender string
	Text   string
}

var (
	// Matches WhatsApp timestamp prefixes:
	// e.g. "11/10/2026, 8:15 PM - " or "[14:22, 05/03/2026] " or "2026-11-10, 20:15 - "
	whatsappPrefixRe = regexp.MustCompile(`^(?:\[?\d{1,4}[./-]\d{1,2}[./-]\d{2,4},?\s+\d{1,2}:\d{2}(?::\d{2})?(?:\s*[APap][Mm])?\]?\s*[-\x{2013}]\s*|\[\d{1,2}:\d{2}(?::\d{2})?,\s*\d{1,2}[./-]\d{1,2}[./-]\d{2,4}\]\s*)`)
	// Matches plain dialogue lines like: "Sahand: message" or "[Sahand] message"
	plainLineRe = regexp.MustCompile(`^(?:\[([^\]]{1,50})\]|([^:\[\]]{1,50}):)\s*(.+)$`)
)

// ParseChatExport extracts ordered (Sender, Text) turns from Telegram JSON, WhatsApp .txt, or plain transcripts.
func ParseChatExport(raw string) ([]chatTurn, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "empty"
	}

	// 1. Try Telegram Desktop JSON export ("messages": [{"from": "...", "text": ...}])
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if turns, ok := parseTelegramJSON(trimmed); ok && len(turns) > 0 {
			return mergeConsecutiveTurns(turns), "telegram_json"
		}
	}

	// 2. Try WhatsApp / Plain line-by-line dialogue
	lines := strings.Split(trimmed, "\n")
	var turns []chatTurn
	detected := "plain_transcript"

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		// Strip WhatsApp date/time header first if present
		if loc := whatsappPrefixRe.FindStringIndex(line); loc != nil {
			rest := strings.TrimSpace(line[loc[1]:])
			if colonIdx := strings.Index(rest, ":"); colonIdx > 0 && colonIdx <= 60 {
				detected = "whatsapp_txt"
				turns = append(turns, chatTurn{
					Sender: strings.TrimSpace(rest[:colonIdx]),
					Text:   strings.TrimSpace(rest[colonIdx+1:]),
				})
				continue
			}
		}

		if m := plainLineRe.FindStringSubmatch(line); len(m) == 4 {
			sender := strings.TrimSpace(m[1])
			if sender == "" {
				sender = strings.TrimSpace(m[2])
			}
			turns = append(turns, chatTurn{
				Sender: sender,
				Text:   strings.TrimSpace(m[3]),
			})
			continue
		}

		// Continuation line of the previous turn
		if len(turns) > 0 {
			turns[len(turns)-1].Text += "\n" + line
		}
	}

	return mergeConsecutiveTurns(turns), detected
}

func parseTelegramJSON(raw string) ([]chatTurn, bool) {
	var root struct {
		Messages []struct {
			Type string `json:"type"`
			From string `json:"from"`
			Text any    `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &root); err != nil || len(root.Messages) == 0 {
		return nil, false
	}

	turns := make([]chatTurn, 0, len(root.Messages))
	for _, msg := range root.Messages {
		if msg.Type != "" && msg.Type != "message" {
			continue
		}
		sender := strings.TrimSpace(msg.From)
		if sender == "" {
			continue
		}
		txt := flattenTelegramText(msg.Text)
		if txt == "" {
			continue
		}
		turns = append(turns, chatTurn{Sender: sender, Text: txt})
	}
	return turns, true
}

func flattenTelegramText(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case []any:
		var sb strings.Builder
		for _, item := range val {
			switch part := item.(type) {
			case string:
				sb.WriteString(part)
			case map[string]any:
				if t, ok := part["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return strings.TrimSpace(sb.String())
	default:
		return ""
	}
}

func mergeConsecutiveTurns(turns []chatTurn) []chatTurn {
	if len(turns) == 0 {
		return nil
	}
	merged := make([]chatTurn, 0, len(turns))
	for _, t := range turns {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		if len(merged) > 0 && strings.EqualFold(merged[len(merged)-1].Sender, t.Sender) {
			merged[len(merged)-1].Text += "\n" + t.Text
		} else {
			merged = append(merged, t)
		}
	}
	return merged
}

// ExtractTargetPairs filters parsed dialogue turns so only replies written by targetName
// (paired with the preceding message they were replying to) are extracted.
func ExtractTargetPairs(turns []chatTurn, targetName string, maxPairs int) ([]FewShotExample, int) {
	if maxPairs <= 0 {
		maxPairs = 24
	}
	targetLower := strings.ToLower(strings.TrimSpace(targetName))
	var pairs []FewShotExample
	matchedReplies := 0

	for i, t := range turns {
		senderLower := strings.ToLower(strings.TrimSpace(t.Sender))
		isTarget := targetLower == "" ||
			senderLower == targetLower ||
			strings.Contains(senderLower, targetLower) ||
			senderLower == "me" ||
			senderLower == "self"
		if !isTarget {
			continue
		}
		matchedReplies++
		incoming := "Hey, what's up?"
		if i > 0 && !strings.EqualFold(turns[i-1].Sender, t.Sender) {
			incoming = turns[i-1].Text
		}
		pairs = append(pairs, FewShotExample{
			User:      truncateRunes(incoming, 400),
			Assistant: truncateRunes(t.Text, 600),
		})
	}

	if len(pairs) <= maxPairs {
		return pairs, matchedReplies
	}

	// Sample evenly across the entire chat history for diversity
	sampled := make([]FewShotExample, 0, maxPairs)
	step := float64(len(pairs)) / float64(maxPairs)
	for i := 0; i < maxPairs; i++ {
		idx := int(float64(i) * step)
		if idx >= len(pairs) {
			idx = len(pairs) - 1
		}
		sampled = append(sampled, pairs[idx])
	}
	return sampled, matchedReplies
}

// DistillVoicePersona analyzes the user's real chat log and uses an LLM to synthesize
// a reusable Voice Blueprint + curated few-shot examples without leaking private secrets.
func DistillVoicePersona(
	ctx context.Context,
	router *provider.Router,
	keyLookup func(string) string,
	req TrainRequest,
) (*TrainResult, error) {
	if router == nil {
		return nil, errors.New("provider router not initialized")
	}
	rawLog := strings.TrimSpace(req.RawChatLog)
	if rawLog == "" {
		return nil, errors.New("please paste or upload chat messages to train from")
	}
	target := strings.TrimSpace(req.TargetName)
	if target == "" {
		target = "Me"
	}
	maxExamples := req.MaxExamples
	if maxExamples <= 0 || maxExamples > 10 {
		maxExamples = 6
	}

	turns, format := ParseChatExport(rawLog)
	pairs, matchedCount := ExtractTargetPairs(turns, target, 28)

	// Fallback if the user pasted raw standalone sentences without speaker labels
	if len(pairs) == 0 {
		lines := strings.Split(rawLog, "\n")
		for _, l := range lines {
			clean := strings.TrimSpace(l)
			if clean != "" {
				pairs = append(pairs, FewShotExample{
					User:      "(conversation prompt)",
					Assistant: truncateRunes(clean, 500),
				})
				matchedCount++
				if len(pairs) >= 24 {
					break
				}
			}
		}
		format = "raw_lines"
	}

	if len(pairs) == 0 {
		return nil, errors.New("could not find any usable messages in the provided chat log")
	}

	var sampleSB strings.Builder
	for i, p := range pairs {
		fmt.Fprintf(&sampleSB, "--- Exchange %d ---\nOther person: %s\n%s's actual reply: %s\n\n", i+1, p.User, target, p.Assistant)
	}

	sysPrompt := `You are an expert linguistic profiler and persona architect.
Your job is to study real chat messages written by a specific person ("Target Speaker") and distill their exact communication style into a structured Voice Blueprint so an AI can convincingly talk and act like them.

CRITICAL RULES:
1. Capture concrete stylistic habits: typical message length, sentence structure, punctuation/capitalization quirks, emoji usage, humor/sarcasm style, directness, and how they mix languages (such as Persian/Finglish and English).
2. Do NOT include sensitive personal data (passwords, phone numbers, addresses, financial info, private secrets) in the blueprint or examples.
3. Instruct the persona never to sound like a generic polite AI assistant ("How can I help you today?", "As an AI...").
4. Output ONLY valid JSON matching this exact schema (no markdown fences):
{
  "voice_blueprint": "Detailed second-person instructions ('You are [Name]. Talk like...') covering tone, rhythm, vocabulary, bilingual habits, humor, and boundaries.",
  "few_shot_examples": [
    {"user": "short realistic incoming message", "assistant": "reply written in the exact authentic voice of the target speaker"}
  ]
}`

	userPrompt := fmt.Sprintf("Target Speaker Name: %s\nExtra Style Notes: %s\nDesired Few-Shot Examples Count: %d\n\nHere are %d real message exchanges from %s:\n\n%s",
		target,
		strings.TrimSpace(req.ExtraNotes),
		maxExamples,
		len(pairs),
		target,
		sampleSB.String(),
	)

	prov := provider.CanonicalID(req.Provider)
	mod := strings.TrimSpace(req.Model)
	if prov == "" || mod == "" {
		return nil, errors.New("please select a provider and model to run the voice trainer")
	}

	rawReply, _, err := router.Call(ctx, prov, keyLookup(prov), mod, []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}, provider.CallOpts{MaxTokens: 2048})
	if err != nil {
		return nil, fmt.Errorf("voice trainer model call failed: %w", err)
	}

	cleanJSON := stripCodeFences(rawReply)
	var parsed struct {
		VoiceBlueprint  string           `json:"voice_blueprint"`
		FewShotExamples []FewShotExample `json:"few_shot_examples"`
	}
	if err := json.Unmarshal([]byte(cleanJSON), &parsed); err != nil || strings.TrimSpace(parsed.VoiceBlueprint) == "" {
		// Graceful fallback if the model returned plain prose instead of strict JSON
		fallbackExamples := pairs
		if len(fallbackExamples) > maxExamples {
			fallbackExamples = fallbackExamples[:maxExamples]
		}
		return &TrainResult{
			VoiceBlueprint:  strings.TrimSpace(rawReply),
			FewShotExamples: fallbackExamples,
			ParsedTurns:     len(turns),
			MatchedReplies:  matchedCount,
			DetectedFormat:  format,
		}, nil
	}

	examples := cleanFewShotExamples(parsed.FewShotExamples)
	if len(examples) == 0 {
		examples = pairs
		if len(examples) > maxExamples {
			examples = examples[:maxExamples]
		}
	}

	return &TrainResult{
		VoiceBlueprint:  strings.TrimSpace(parsed.VoiceBlueprint),
		FewShotExamples: examples,
		ParsedTurns:     len(turns),
		MatchedReplies:  matchedCount,
		DetectedFormat:  format,
	}, nil
}

func stripCodeFences(s string) string {
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

func truncateRunes(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
