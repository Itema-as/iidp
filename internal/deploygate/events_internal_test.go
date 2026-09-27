package deploygate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The API server refuses an Event whose note is over 1024 bytes, and a
// refusal's reason is the gate's own message, so a long one is cut on a
// character boundary.
func TestTruncateKeepsNotesWithinTheAPIServersLimit(t *testing.T) {
	if got := truncate("short", noteLimit); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
	long := strings.Repeat("æ", 700) // 1400 bytes
	got := truncate(long, noteLimit)
	if len(got) > noteLimit || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate = %d bytes, valid UTF-8 %t, want at most %d ending in …", len(got), utf8.ValidString(got), noteLimit)
	}
}
