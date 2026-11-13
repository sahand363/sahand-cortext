package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrUnknownProvider       = errors.New("unknown AI provider")
	ErrDuplicateProvider     = errors.New("provider ID is already registered")
	ErrInvalidConfig         = errors.New("invalid provider configuration")
	ErrBuiltInProtected      = errors.New("built-in providers cannot be deleted or overwritten")
	ErrProviderDisabled      = errors.New("provider is disabled")
	ErrUnsupportedCapability = errors.New("provider does not support requested capability")
)

func ClassifyProviderError(err error, elapsedMs int64) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	sec := float64(elapsedMs) / 1000.0
	msg := strings.ToLower(err.Error())

	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return fmt.Errorf("TIMEOUT after %.1fs (model took too long to finish generating)", sec)
	}
	if strings.Contains(msg, "connection reset by peer") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "unexpected eof") {
		return fmt.Errorf("CONNECTION RESET after %.1fs (upstream proxy closed connection): %v", sec, err)
	}
	if strings.Contains(msg, "http 429") {
		return fmt.Errorf("RATE LIMIT (HTTP 429 — provider throttled requests): %v", err)
	}
	if strings.Contains(msg, "http 401") || strings.Contains(msg, "http 402") || strings.Contains(msg, "http 403") {
		return fmt.Errorf("AUTH / WALLET / QUOTA ERROR: %v", err)
	}
	if strings.Contains(msg, "http 500") || strings.Contains(msg, "http 502") || strings.Contains(msg, "http 503") || strings.Contains(msg, "http 504") {
		return fmt.Errorf("PROVIDER OVERLOADED (upstream server error): %v", err)
	}
	if strings.Contains(msg, "exhausted output tokens during internal reasoning") {
		return fmt.Errorf("REASONING TRUNCATED after %.1fs: %v", sec, err)
	}
	if strings.Contains(msg, "empty content") || strings.Contains(msg, "empty response") {
		return fmt.Errorf("EMPTY RESPONSE (model returned 0 tokens): %v", err)
	}
	return err
}

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnknownProvider) || errors.Is(err, ErrInvalidConfig) ||
		errors.Is(err, ErrProviderDisabled) || errors.Is(err, ErrUnsupportedCapability) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 429") ||
		strings.Contains(msg, "http 500") ||
		strings.Contains(msg, "http 502") ||
		strings.Contains(msg, "http 503") ||
		strings.Contains(msg, "http 504") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "unexpected end of json") ||
		strings.Contains(msg, "invalid json response") ||
		strings.Contains(msg, "empty content") ||
		strings.Contains(msg, "empty response") ||
		strings.Contains(msg, "exhausted output tokens during internal reasoning") ||
		strings.Contains(msg, "eof")
}

func ExtractAPIError(raw []byte) string {
	var eWrap struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &eWrap) == nil {
		switch v := eWrap.Error.(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if msg, ok := v["message"].(string); ok && msg != "" {
				return msg
			}
		}
		if eWrap.Message != "" {
			return eWrap.Message
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}
