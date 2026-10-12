package remote

import "testing"


func TestWithPathPrefixEmpty(t *testing.T) {
	got := withPathPrefix("", "printenv HOME")
	if got != "printenv HOME" {
		t.Fatalf("withPathPrefix empty = %q", got)
	}
	got = withPathPrefix("export PATH=$HOME/.opencode/bin:$PATH", "printenv HOME")
	want := "export PATH=$HOME/.opencode/bin:$PATH; printenv HOME"
	if got != want {
		t.Fatalf("withPathPrefix = %q, want %q", got, want)
	}
	got = withPathPrefix("   ", "x")
	if got != "x" {
		t.Fatalf("withPathPrefix blank = %q", got)
	}
}
