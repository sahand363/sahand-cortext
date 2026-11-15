package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

// ExtractFromMessages asks the model to list durable facts worth remembering
// from a conversation. existing are facts already stored, so the model only
// returns NEW ones. It returns at most 10 facts, each a plain sentence.
// The caller must run this AFTER closing every DB row: it makes a long AI
// call, and on the single-connection SQLite a held row would deadlock it.
func ExtractFromMessages(ctx context.Context, router *provider.Router, apiKey, providerID, modelID string, existing []string, messages []provider.Message) ([]string, error) {
	if router == nil || strings.TrimSpace(apiKey) == "" || strings.TrimSpace(modelID) == "" {
		return nil, fmt.Errorf("no provider configured for memory extraction")
	}
	if len(messages) == 0 {
		return nil, nil
	}
	var conv strings.Builder
	for _, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		conv.WriteString(m.Role)
		conv.WriteString(": ")
		conv.WriteString(m.Content)
		conv.WriteString("\n")
	}
	convText := conv.String()
	if len([]rune(convText)) > 12000 {
		r := []rune(convText)
		convText = string(r[len(r)-12000:])
	}
	var existingBlock strings.Builder
	if len(existing) > 0 {
		existingBlock.WriteString("Facts already remembered (do NOT repeat these):\n")
		for _, e := range existing {
			existingBlock.WriteString("- " + strings.TrimSpace(e) + "\n")
		}
	} else {
		existingBlock.WriteString("No facts remembered yet.\n")
	}
	sysPrompt := "You are a memory extractor. Read the conversation and list durable facts worth remembering across sessions: decisions the user made, preferences they stated, commitments, project state, and facts about the user and their projects. Write ONE fact per line, as a plain sentence, no bullets, no numbering, no commentary. Only include facts that are durable (true beyond this conversation) and not already in the list below. If nothing is worth remembering, reply with exactly: NONE."
	userPrompt := existingBlock.String() + "\nConversation:\n" + convText + "\n\nDurable facts (one per line, or NONE):"
	answer, _, err := router.Call(ctx, providerID, apiKey, modelID, []provider.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}, provider.CallOpts{})
	if err != nil {
		return nil, err
	}
	return parseFacts(answer, existing), nil
}

// parseFacts turns the model's reply into a clean list of facts.
func parseFacts(answer string, existing []string) []string {
	seen := map[string]bool{}
	for _, e := range existing {
		seen[strings.ToLower(strings.TrimSpace(e))] = true
	}
	var out []string
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "• ")
		line = strings.TrimSpace(strings.TrimPrefix(line, "* "))
		if line == "" || strings.EqualFold(line, "NONE") {
			continue
		}
		// Strip a leading "1. " / "2. " numbering if the model added it.
		if len(line) > 2 && line[0] >= '0' && line[0] <= '9' && line[1] == '.' {
			line = strings.TrimSpace(line[2:])
		}
		if line == "" || len([]rune(line)) > 500 {
			continue
		}
		key := strings.ToLower(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, line)
		if len(out) >= 10 {
			break
		}
	}
	return out
}
