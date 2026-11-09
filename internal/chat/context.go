package chat

import (
	"strings"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

// HistoryConfig controls conversational history limits
type HistoryConfig struct {
	MaxMessages int
	MaxChars    int
}

// DefaultConfig returns default context limits (20 turns or 32,000 characters)
func DefaultConfig() HistoryConfig {
	return HistoryConfig{
		MaxMessages: 20,
		MaxChars:    32000,
	}
}

// TruncateHistory ensures the history fits within token/character budgets
// while preserving initial system instructions and the most recent conversational turns.
func TruncateHistory(messages []provider.Message, cfg HistoryConfig) []provider.Message {
	if len(messages) == 0 {
		return messages
	}
	if cfg.MaxMessages <= 0 {
		cfg.MaxMessages = 20
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = 32000
	}

	var systemMsg *provider.Message
	filtered := make([]provider.Message, 0, len(messages))

	for i, m := range messages {
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		if i == 0 && m.Role == "system" {
			systemMsg = &provider.Message{Role: "system", Content: c}
			continue
		}
		filtered = append(filtered, provider.Message{Role: m.Role, Content: c})
	}

	// 1. Message-count limit from recent back
	if len(filtered) > cfg.MaxMessages {
		filtered = filtered[len(filtered)-cfg.MaxMessages:]
	}

	// 2. Character-budget limit from recent back
	totalChars := 0
	if systemMsg != nil {
		totalChars += len(systemMsg.Content)
	}

	cutIndex := 0
	for i := len(filtered) - 1; i >= 0; i-- {
		totalChars += len(filtered[i].Content)
		if totalChars > cfg.MaxChars {
			cutIndex = i + 1
			break
		}
	}
	filtered = filtered[cutIndex:]

	// 3. Ensure conversation begins with a user turn
	if len(filtered) > 0 && filtered[0].Role != "user" {
		filtered = filtered[1:]
	}

	var result []provider.Message
	if systemMsg != nil {
		result = append(result, *systemMsg)
	}
	result = append(result, filtered...)
	return result
}
