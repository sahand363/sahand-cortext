package main

import "testing"

func TestUnstreamedRemainder(t *testing.T) {
	cases := []struct{ streamed, answer, want string }{
		{"", "Hello", "Hello"},                 // nothing streamed (reasoning salvage)
		{"Hello", "Hello", ""},                 // fully streamed
		{"Hel", "Hello", "lo"},                 // partially streamed
		{"\n\nHello", "Hello world", " world"}, // answer is trimmed
		{"abc", "xyz", ""},                     // diverged: nothing safe to add
		{"anything", "", ""},
	}
	for _, c := range cases {
		if got := unstreamedRemainder(c.streamed, c.answer); got != c.want {
			t.Errorf("unstreamedRemainder(%q, %q) = %q, want %q", c.streamed, c.answer, got, c.want)
		}
	}
}
