package provider

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPError is a non-2xx reply from a provider. Its Error() text keeps the
// "<Name> HTTP <code>: <msg>" shape every classifier matches on, and it
// carries how long the provider asked us to wait, when it said.
type HTTPError struct {
	Provider   string
	Status     int
	Message    string
	RetryAfter time.Duration // 0 when the provider gave no hint
	DailyQuota bool          // the quota that ran out is a per-day one
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s HTTP %d: %s", e.Provider, e.Status, e.Message)
}

var retryDelayRe = regexp.MustCompile(`"retryDelay"\s*:\s*"([0-9.]+)s"`)
var retryInTextRe = regexp.MustCompile(`(?i)(?:retry|try again) in ([0-9.]+)\s*(ms|s|sec|seconds?)\b`)

// NewHTTPError builds an HTTPError from a response and its (already read) body.
func NewHTTPError(providerName string, resp *http.Response, body []byte) *HTTPError {
	e := &HTTPError{Provider: providerName, Status: resp.StatusCode, Message: ExtractAPIError(body)}
	e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	if e.RetryAfter == 0 {
		if m := retryDelayRe.FindSubmatch(body); m != nil {
			if f, err := strconv.ParseFloat(string(m[1]), 64); err == nil {
				e.RetryAfter = time.Duration(f * float64(time.Second))
			}
		}
	}
	if e.RetryAfter == 0 {
		if m := retryInTextRe.FindStringSubmatch(string(body)); m != nil {
			if f, err := strconv.ParseFloat(m[1], 64); err == nil {
				if strings.ToLower(m[2]) == "ms" {
					e.RetryAfter = time.Duration(f * float64(time.Millisecond))
				} else {
					e.RetryAfter = time.Duration(f * float64(time.Second))
				}
			}
		}
	}
	lb := strings.ToLower(string(body))
	if resp.StatusCode == 429 && (strings.Contains(lb, "perday") || strings.Contains(lb, "per day") ||
		strings.Contains(lb, "requests per day") || strings.Contains(lb, "daily")) {
		e.DailyQuota = true
	}
	return e
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// AsHTTPError unwraps an HTTPError from err, if there is one.
func AsHTTPError(err error) (*HTTPError, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

// IsRateLimit reports whether err is an HTTP 429.
func IsRateLimit(err error) bool {
	if err == nil {
		return false
	}
	if he, ok := AsHTTPError(err); ok {
		return he.Status == 429
	}
	return strings.Contains(strings.ToLower(err.Error()), "http 429")
}

// IsOverloaded reports whether err is a 5xx server error.
func IsOverloaded(err error) bool {
	if err == nil {
		return false
	}
	if he, ok := AsHTTPError(err); ok {
		return he.Status >= 500 && he.Status <= 599
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "http 500") || strings.Contains(m, "http 502") ||
		strings.Contains(m, "http 503") || strings.Contains(m, "http 504")
}

// Jitter adds a random 0-30% to d so parallel callers don't retry in lockstep.
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(d)*3/10+1))
}

// ---- Per provider/model cooldowns (a small circuit breaker) ----

const (
	DefaultRateLimitCooldown = 60 * time.Second
	OverloadCooldown         = 90 * time.Second
	MaxCooldown              = 10 * time.Minute
	DailyQuotaCooldown       = 6 * time.Hour
)

type cooldownEntry struct {
	until  time.Time
	reason string
}

var (
	cooldownMu sync.Mutex
	cooldowns  = map[string]cooldownEntry{}
)

func cooldownKey(providerID, model string) string {
	return strings.ToLower(strings.TrimSpace(providerID)) + "|" + strings.TrimSpace(model)
}

// MarkCooldown parks providerID/model for d. A longer existing cooldown wins.
func MarkCooldown(providerID, model string, d time.Duration, reason string) {
	if d <= 0 {
		return
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	k := cooldownKey(providerID, model)
	until := time.Now().Add(d)
	if cur, ok := cooldowns[k]; ok && cur.until.After(until) {
		return
	}
	cooldowns[k] = cooldownEntry{until: until, reason: reason}
}

// CooldownRemaining returns how long providerID/model is still parked, and why.
func CooldownRemaining(providerID, model string) (time.Duration, string) {
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	k := cooldownKey(providerID, model)
	e, ok := cooldowns[k]
	if !ok {
		return 0, ""
	}
	d := time.Until(e.until)
	if d <= 0 {
		delete(cooldowns, k)
		return 0, ""
	}
	return d, e.reason
}

// ClearCooldown forgets a cooldown, after the model answered successfully.
func ClearCooldown(providerID, model string) {
	cooldownMu.Lock()
	delete(cooldowns, cooldownKey(providerID, model))
	cooldownMu.Unlock()
}

// CooldownFor decides how long to park a model after err, or 0 for no cooldown.
func CooldownFor(err error) time.Duration {
	if IsRateLimit(err) {
		if he, ok := AsHTTPError(err); ok {
			if he.DailyQuota {
				return DailyQuotaCooldown
			}
			if he.RetryAfter > 0 {
				if he.RetryAfter > MaxCooldown {
					return MaxCooldown
				}
				return he.RetryAfter
			}
		}
		return DefaultRateLimitCooldown
	}
	if IsOverloaded(err) {
		return OverloadCooldown
	}
	return 0
}
