package api

import (
	"strings"
	"testing"
)

func TestApplyUIManifest(t *testing.T) {
	const base = `{"name":"MindFS","short_name":"MindFS","start_url":"/","icons":[]}`

	tests := []struct {
		name      string
		title     string
		want      string
		untouched bool
	}{
		{name: "default keeps manifest as-is", title: "MindFS", untouched: true},
		{name: "empty keeps manifest as-is", title: "", untouched: true},
		{name: "custom title replaces both names", title: "MindFS-TX", want: "MindFS-TX"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &HTTPHandler{UITitle: tc.title}
			got := string(h.applyUIManifest([]byte(base)))
			if tc.untouched {
				if got != base {
					t.Fatalf("expected untouched manifest, got %q", got)
				}
				return
			}
			if !strings.Contains(got, `"name":"MindFS-TX"`) ||
				!strings.Contains(got, `"short_name":"MindFS-TX"`) {
				t.Fatalf("expected both names replaced, got %q", got)
			}
			// Non-name fields must survive the rewrite.
			if !strings.Contains(got, `"start_url":"/"`) {
				t.Fatalf("expected other fields preserved, got %q", got)
			}
		})
	}
}

func TestApplyUIManifestMalformedPassesThrough(t *testing.T) {
	h := &HTTPHandler{UITitle: "MindFS-TX"}
	bad := []byte("not json")
	if got := string(h.applyUIManifest(bad)); got != string(bad) {
		t.Fatalf("expected malformed manifest untouched, got %q", got)
	}
}
