package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestNewHTTPErrorRetryHints(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"12"}}}
	e := NewHTTPError("X", resp, []byte(`{"error":{"message":"slow down"}}`))
	if e.RetryAfter != 12*time.Second {
		t.Fatalf("Retry-After header: got %v", e.RetryAfter)
	}
	resp = &http.Response{StatusCode: 429, Header: http.Header{}}
	body := []byte(`{"error":{"code":429,"message":"Quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37s"}]}}`)
	e = NewHTTPError("Gemini", resp, body)
	if e.RetryAfter != 37*time.Second {
		t.Fatalf("Gemini retryDelay: got %v", e.RetryAfter)
	}
	daily := []byte(`{"error":{"message":"Quota exceeded for metric generate_content_free_tier_requests, quotaId: GenerateRequestsPerDayPerProjectPerModel-FreeTier"}}`)
	if e = NewHTTPError("Gemini", resp, daily); !e.DailyQuota {
		t.Fatal("expected daily quota")
	}
	if !IsRateLimit(ClassifyProviderError(e, 10)) {
		t.Fatal("classified error lost its 429 type")
	}
}

func TestRetryWaitPolicy(t *testing.T) {
	rl := &HTTPError{Status: 429, RetryAfter: 3 * time.Second}
	if _, again := retryWait(0, rl, CallOpts{FailFastOnRateLimit: true}); again {
		t.Fatal("fail-fast 429 must not wait")
	}
	if w, again := retryWait(0, rl, CallOpts{}); !again || w < 3*time.Second {
		t.Fatalf("429 without backup should wait Retry-After, got %v %v", w, again)
	}
	if _, again := retryWait(0, &HTTPError{Status: 429, RetryAfter: time.Minute}, CallOpts{}); again {
		t.Fatal("long Retry-After should park, not wait")
	}
	ov := &HTTPError{Status: 503}
	if _, again := retryWait(1, ov, CallOpts{}); !again {
		t.Fatal("5xx gets a second retry")
	}
	if _, again := retryWait(2, ov, CallOpts{}); again {
		t.Fatal("5xx stops after two retries")
	}
	if _, again := retryWait(0, &HTTPError{Status: 401}, CallOpts{}); again {
		t.Fatal("401 never retries")
	}
}

func TestCooldown(t *testing.T) {
	MarkCooldown("p", "m", time.Second, "x")
	if d, _ := CooldownRemaining("p", "m"); d <= 0 {
		t.Fatal("expected cooldown")
	}
	ClearCooldown("p", "m")
	if d, _ := CooldownRemaining("p", "m"); d != 0 {
		t.Fatal("expected cleared")
	}
}
