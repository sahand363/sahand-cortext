package websearch

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Free fallbacks for when DuckDuckGo refuses the server (it answers a
// burst of searches from a server IP with HTTP 202, a bot check, and can
// keep doing so for a while). None needs a key.

// Chain tries each search in order and returns the first that finds
// something. A source that refuses (HTTP 202/403/429, bot check) is
// skipped for a cool-down instead of being hammered again.
func Chain(sources ...NamedSearch) SearchFunc {
	var mu sync.Mutex
	pausedUntil := map[string]time.Time{}
	return func(ctx context.Context, query string, max int) ([]Result, error) {
		var errs []string
		for _, s := range sources {
			mu.Lock()
			until := pausedUntil[s.Name]
			mu.Unlock()
			if time.Now().Before(until) {
				errs = append(errs, s.Name+": paused after a refusal")
				continue
			}
			res, err := s.Search(ctx, query, max)
			if err == nil && len(res) > 0 {
				return res, nil
			}
			if err == nil {
				err = ErrNoResults
			}
			if isRefusal(err) {
				mu.Lock()
				pausedUntil[s.Name] = time.Now().Add(10 * time.Minute)
				mu.Unlock()
			}
			log.Printf("websearch: %s: %v", s.Name, err)
			errs = append(errs, s.Name+": "+err.Error())
			if ctx.Err() != nil {
				break
			}
		}
		if len(errs) == 0 {
			return nil, ErrNoResults
		}
		return nil, errors.New(strings.Join(errs, "; "))
	}
}

// NamedSearch is one source in a Chain.
type NamedSearch struct {
	Name   string
	Search SearchFunc
}

// DefaultSearch is the hub's search: DuckDuckGo, then Google News (RSS),
// then Wikipedia.
func DefaultSearch() SearchFunc {
	return Chain(
		NamedSearch{"duckduckgo", DuckDuckGo},
		NamedSearch{"google-news", GoogleNews},
		NamedSearch{"wikipedia", Wikipedia},
	)
}

func isRefusal(err error) bool {
	m := err.Error()
	return strings.Contains(m, "HTTP 202") || strings.Contains(m, "HTTP 403") ||
		strings.Contains(m, "HTTP 429") || strings.Contains(m, "bot check")
}

func httpGet(ctx context.Context, u, ua string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answered HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}

// isPersian reports whether text is mostly Arabic-script letters.
func isPersian(text string) bool {
	ar, other := 0, 0
	for _, r := range text {
		if unicode.Is(unicode.Arabic, r) {
			ar++
		} else if unicode.IsLetter(r) {
			other++
		}
	}
	return ar > other
}

type gnRSS struct {
	Items []struct {
		Title   string `xml:"title"`
		Link    string `xml:"link"`
		PubDate string `xml:"pubDate"`
		Source  string `xml:"source"`
	} `xml:"channel>item"`
}

// GoogleNews searches the public Google News RSS feed. Its links are
// Google redirects that cannot be read as pages, so the note is written
// from the headlines, sources and dates.
func GoogleNews(ctx context.Context, query string, max int) ([]Result, error) {
	q := url.Values{"q": {query}}
	if isPersian(query) {
		q.Set("hl", "fa")
	} else {
		q.Set("hl", "en-US")
		q.Set("gl", "US")
		q.Set("ceid", "US:en")
	}
	b, err := httpGet(ctx, "https://news.google.com/rss/search?"+q.Encode(), userAgent)
	if err != nil {
		return nil, err
	}
	return parseGoogleNews(b, max)
}

func parseGoogleNews(b []byte, max int) ([]Result, error) {
	var feed gnRSS
	if err := xml.Unmarshal(b, &feed); err != nil {
		return nil, fmt.Errorf("bad news feed: %w", err)
	}
	var out []Result
	for _, it := range feed.Items {
		if it.Link == "" || it.Title == "" {
			continue
		}
		snippet := "News headline"
		if it.Source != "" {
			snippet += " from " + it.Source
		}
		if it.PubDate != "" {
			if t, err := time.Parse(time.RFC1123, it.PubDate); err == nil {
				snippet += ", published " + t.UTC().Format("2006-01-02 15:04 UTC")
			} else {
				snippet += ", published " + it.PubDate
			}
		}
		out = append(out, Result{Title: cleanText(it.Title), URL: it.Link, Snippet: snippet})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// botAgent identifies the hub, as Wikipedia asks of API clients.
const botAgent = "SahandCorText/1.0 (https://github.com/sahand363/sahand-cortext)"

// Wikipedia searches Persian or English Wikipedia (by the query's script).
func Wikipedia(ctx context.Context, query string, max int) ([]Result, error) {
	lang := "en"
	if isPersian(query) {
		lang = "fa"
	}
	q := url.Values{"action": {"query"}, "list": {"search"}, "srsearch": {query}, "format": {"json"}, "srlimit": {fmt.Sprint(max)}}
	b, err := httpGet(ctx, "https://"+lang+".wikipedia.org/w/api.php?"+q.Encode(), botAgent)
	if err != nil {
		return nil, err
	}
	var data struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	var out []Result
	for _, s := range data.Query.Search {
		u := "https://" + lang + ".wikipedia.org/wiki/" + url.PathEscape(strings.ReplaceAll(s.Title, " ", "_"))
		out = append(out, Result{Title: s.Title + " - Wikipedia", URL: u, Snippet: cleanText(s.Snippet)})
	}
	return out, nil
}

// unreadable reports links that cannot be read as pages.
func unreadable(u string) bool {
	return strings.HasPrefix(u, "https://news.google.com/")
}
