// Package websearch gives every chat (web panel, Android app, Telegram
// bots) a free web search with no API key.
//
// It is built around small saved notes instead of raw pages: each search
// reads a few results, has the chat's own model boil them down to a short
// note (key facts, the date, source links), saves that note, and throws
// the pages away. Later turns get the relevant notes in their prompt, and
// when a note is too thin or too old the model asks for a narrower search
// for just that part. So knowledge piles up as a few KB per search, not as
// whole web pages.
//
// The model asks for a search by answering with one line:
//
//	[[SEARCH: short query]]
//
// The hub runs the search, adds the note to the prompt and asks again.
// The user can also force one with "/search <query>".
package websearch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// SearchFunc runs a web search and returns up to max results.
type SearchFunc func(ctx context.Context, query string, max int) ([]Result, error)

// ErrNoResults means the search engine answered but found nothing.
var ErrNoResults = errors.New("the web search found nothing")

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

var (
	liteLinkRe    = regexp.MustCompile(`(?s)<a[^>]+href="([^"]+)"[^>]*class='result-link'[^>]*>(.*?)</a>`)
	liteSnippetRe = regexp.MustCompile(`(?s)<td class='result-snippet'>(.*?)</td>`)
	htmlLinkRe    = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	htmlSnippetRe = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
	anyTagRe      = regexp.MustCompile(`<[^>]+>`)
	spacesRe      = regexp.MustCompile(`\s+`)
)

// DuckDuckGo searches DuckDuckGo's no-JavaScript pages: free, no key, no
// account. It tries the "lite" page first (the "html" page often answers
// server IPs with a bot check), then the "html" page.
func DuckDuckGo(ctx context.Context, query string, max int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty search query")
	}
	if max <= 0 {
		max = 6
	}
	client := &http.Client{Timeout: 15 * time.Second}

	res, err := ddgOnce(ctx, client, query, max)
	if err == nil {
		return res, nil
	}
	if isRefusal(err) {
		return nil, err // refused: let the next source answer
	}
	// DuckDuckGo often refuses a burst of quick searches; wait and try once more.
	select {
	case <-ctx.Done():
		return nil, err
	case <-time.After(2500 * time.Millisecond):
	}
	return ddgOnce(ctx, client, query, max)
}

func ddgOnce(ctx context.Context, client *http.Client, query string, max int) ([]Result, error) {
	var lastErr error
	attempts := []struct {
		method, endpoint string
		parse            func(string) []Result
	}{
		{http.MethodPost, "https://lite.duckduckgo.com/lite/", parseLite},
		{http.MethodGet, "https://lite.duckduckgo.com/lite/", parseLite},
		{http.MethodPost, "https://html.duckduckgo.com/html/", parseHTML},
	}
	for _, at := range attempts {
		body, err := ddgFetch(ctx, client, at.method, at.endpoint, query)
		if err != nil {
			lastErr = err
			continue
		}
		res := at.parse(body)
		if len(res) == 0 {
			lastErr = ErrNoResults
			if strings.Contains(body, "anomaly") || strings.Contains(body, "challenge-form") {
				lastErr = errors.New("DuckDuckGo asked for a bot check (too many searches)")
			}
			continue
		}
		if len(res) > max {
			res = res[:max]
		}
		return res, nil
	}
	if lastErr == nil {
		lastErr = ErrNoResults
	}
	return nil, lastErr
}

func ddgFetch(ctx context.Context, client *http.Client, method, endpoint, query string) (string, error) {
	form := url.Values{"q": {query}, "kl": {"wt-wt"}}
	var req *http.Request
	var err error
	if method == http.MethodPost {
		req, err = http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, method, endpoint+"?"+form.Encode(), nil)
	}
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web search unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("web search answered HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func cleanText(s string) string {
	s = anyTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spacesRe.ReplaceAllString(s, " "))
}

// realURL unwraps DuckDuckGo's redirect links (//duckduckgo.com/l/?uddg=...).
// Ads (y.js links) and anything that is not plain http(s) return "".
func realURL(href string) string {
	href = html.UnescapeString(strings.TrimSpace(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if strings.HasSuffix(u.Host, "duckduckgo.com") {
		if strings.HasPrefix(u.Path, "/y.js") {
			return ""
		}
		if target := u.Query().Get("uddg"); target != "" {
			href = target
			u, err = url.Parse(href)
			if err != nil {
				return ""
			}
		} else {
			return ""
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if strings.HasSuffix(u.Host, "duckduckgo.com") {
		return ""
	}
	return u.String()
}

func parseLite(body string) []Result {
	links := liteLinkRe.FindAllStringSubmatchIndex(body, -1)
	var out []Result
	seen := map[string]bool{}
	for i, m := range links {
		href := body[m[2]:m[3]]
		title := cleanText(body[m[4]:m[5]])
		u := realURL(href)
		if u == "" || seen[u] {
			continue
		}
		// The snippet sits between this link and the next one.
		end := len(body)
		if i+1 < len(links) {
			end = links[i+1][0]
		}
		snippet := ""
		if sm := liteSnippetRe.FindStringSubmatch(body[m[1]:end]); sm != nil {
			snippet = cleanText(sm[1])
		}
		seen[u] = true
		out = append(out, Result{Title: title, URL: u, Snippet: snippet})
	}
	return out
}

func parseHTML(body string) []Result {
	links := htmlLinkRe.FindAllStringSubmatchIndex(body, -1)
	var out []Result
	seen := map[string]bool{}
	for i, m := range links {
		u := realURL(body[m[2]:m[3]])
		if u == "" || seen[u] {
			continue
		}
		end := len(body)
		if i+1 < len(links) {
			end = links[i+1][0]
		}
		snippet := ""
		if sm := htmlSnippetRe.FindStringSubmatch(body[m[1]:end]); sm != nil {
			snippet = cleanText(sm[1])
		}
		seen[u] = true
		out = append(out, Result{Title: cleanText(body[m[4]:m[5]]), URL: u, Snippet: snippet})
	}
	return out
}
