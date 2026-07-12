package tailer

import "testing"

// DESIGN/02: a recognizable level token is detected and normalized to
// uppercase (WARNING -> WARN); anything else is left empty rather than
// guessed.
func TestDetectLevel(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"level=warn msg=\"disk almost full\"", "WARN"},
		{"[ERROR] connection refused", "ERROR"},
		{"INFO: server started on :8080", "INFO"},
		{"2026-07-12T00:00:00Z WARNING deprecated flag used", "WARN"},
		{"debug: retrying in 250ms", "DEBUG"},
		{"trace: entering handler", "TRACE"},
		{"FATAL unrecoverable state", "FATAL"},
		{"panic: index out of range", ""},
		{"just a plain line with no level", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := detectLevel(c.line); got != c.want {
			t.Errorf("detectLevel(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// A level token embedded inside a larger word, with no separating
// punctuation or whitespace, must not match — "reinforcement" contains
// the substring "info" but isn't the word "info".
func TestDetectLevel_WordBoundary(t *testing.T) {
	if got := detectLevel("reinforcement learning is fun"); got != "" {
		t.Errorf("detectLevel should not match a level token as a substring of another word, got %q", got)
	}
}
