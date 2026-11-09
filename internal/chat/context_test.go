package chat

import (
	"testing"

	"github.com/sahand363/sahand-cortext/internal/provider"
)

func TestTruncateHistoryPreservesSystemAndRecent(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are a concise AI assistant."},
		{Role: "user", Content: "Hello 1"},
		{Role: "assistant", Content: "Reply 1"},
		{Role: "user", Content: "Hello 2"},
		{Role: "assistant", Content: "Reply 2"},
		{Role: "user", Content: "Hello 3"},
		{Role: "assistant", Content: "Reply 3"},
		{Role: "user", Content: "Hello 4"},
	}

	cfg := HistoryConfig{
		MaxMessages: 4,
		MaxChars:    10000,
	}

	truncated := TruncateHistory(msgs, cfg)

	// Expect system prompt + 4 most recent messages = 5 total
	if len(truncated) != 5 {
		t.Fatalf("expected 5 messages, got %d: %+v", len(truncated), truncated)
	}
	if truncated[0].Role != "system" || truncated[0].Content != "You are a concise AI assistant." {
		t.Errorf("system instruction missing or corrupted: %+v", truncated[0])
	}
	if truncated[len(truncated)-1].Content != "Hello 4" {
		t.Errorf("latest turn missing: %+v", truncated[len(truncated)-1])
	}
}

func TestSessionIsolation(t *testing.T) {
	sessionA := []provider.Message{
		{Role: "user", Content: "Session A confidential query"},
		{Role: "assistant", Content: "Session A answer"},
	}
	sessionB := []provider.Message{
		{Role: "user", Content: "Session B unrelated query"},
		{Role: "assistant", Content: "Session B answer"},
	}

	outA := TruncateHistory(sessionA, DefaultConfig())
	outB := TruncateHistory(sessionB, DefaultConfig())

	for _, m := range outA {
		if m.Content == "Session B unrelated query" {
			t.Errorf("data leak: session B found in session A context")
		}
	}
	for _, m := range outB {
		if m.Content == "Session A confidential query" {
			t.Errorf("data leak: session A found in session B context")
		}
	}
}

func TestCharacterLimitTruncation(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "Short prefix"},
		{Role: "assistant", Content: "Long text: 1234567890 1234567890 1234567890"},
		{Role: "user", Content: "Latest short turn"},
	}

	cfg := HistoryConfig{
		MaxMessages: 10,
		MaxChars:    25, // Forces dropping the large middle message
	}

	truncated := TruncateHistory(msgs, cfg)
	if len(truncated) != 1 {
		t.Fatalf("expected 1 message within 25 char limit, got %d", len(truncated))
	}
	if truncated[0].Content != "Latest short turn" {
		t.Errorf("expected latest turn, got: %s", truncated[0].Content)
	}
}
