package websearch

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestChainFallsBackAndPausesRefusingSource(t *testing.T) {
	ddgCalls := 0
	s := Chain(
		NamedSearch{"ddg", func(ctx context.Context, q string, max int) ([]Result, error) {
			ddgCalls++
			return nil, errors.New("web search answered HTTP 202")
		}},
		NamedSearch{"news", func(ctx context.Context, q string, max int) ([]Result, error) {
			return []Result{{Title: "t", URL: "https://example.com"}}, nil
		}},
	)
	for i := 0; i < 3; i++ {
		res, err := s(context.Background(), "iran news", 6)
		if err != nil || len(res) != 1 {
			t.Fatalf("run %d: %v %v", i, res, err)
		}
	}
	if ddgCalls != 1 {
		t.Fatalf("refusing source should be paused, called %d times", ddgCalls)
	}
}

func TestParseGoogleNewsAndScript(t *testing.T) {
	feed := `<?xml version="1.0"?><rss><channel><item><title>Big story - Al Jazeera</title><link>https://news.google.com/rss/articles/abc</link><pubDate>Sat, 21 Nov 2026 10:00:00 GMT</pubDate><source url="https://aljazeera.com">Al Jazeera</source></item></channel></rss>`
	res, err := parseGoogleNews([]byte(feed), 6)
	if err != nil || len(res) != 1 || res[0].Snippet != "News headline from Al Jazeera, published 2026-11-21 10:00 UTC" {
		t.Fatalf("%+v %v", res, err)
	}
	if !unreadable(res[0].URL) {
		t.Fatal("google news links must not be fetched")
	}
	if !isPersian("اخبار ایران امروز") || isPersian("iran news today") {
		t.Fatal("script detection")
	}
}

func TestLiveFallbacks(t *testing.T) {
	if os.Getenv("WEBSEARCH_LIVE") == "" {
		t.Skip("set WEBSEARCH_LIVE=1")
	}
	for _, f := range []SearchFunc{GoogleNews, Wikipedia} {
		for _, q := range []string{"iran news", "اخبار ایران"} {
			res, err := f(context.Background(), q, 6)
			if err != nil || len(res) == 0 {
				t.Errorf("%q: %v", q, err)
				continue
			}
			t.Logf("%q -> %s | %s", q, res[0].Title, res[0].Snippet)
		}
	}
}
