package lxmfgroup

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSplitShortTextIsOnePartWithoutAMark(t *testing.T) {
	for _, s := range []string{"", "Rab: hi", strings.Repeat("x", MaxContent)} {
		got := Split(s, MaxContent)
		if len(got) != 1 || got[0] != s {
			t.Errorf("Split(%d bytes) = %q", len(s), got)
		}
	}
}

func TestSplitPrefersParagraphsThenLinesThenSpaces(t *testing.T) {
	para := strings.Repeat("a", 30)
	got := Split(para+"\n\n"+para+" "+para, 70)
	want := []string{"(1/2) " + para, "(2/2) " + para + " " + para}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paragraphs: got %q, want %q", got, want)
	}

	got = Split("one two\nthree four five six seven", 25)
	if got[0] != "(1/3) one two" {
		t.Errorf("line break not preferred: %q", got)
	}

	got = Split("aaaa bbbb cccc dddd eeee ffff", 20)
	for _, p := range got {
		if strings.HasSuffix(p, " ") || strings.Contains(p, ") ") && strings.HasPrefix(p[strings.Index(p, ") ")+2:], " ") {
			t.Errorf("whitespace left at a cut: %q", p)
		}
	}
}

func TestSplitNeverCutsInsideACharacter(t *testing.T) {
	s := strings.Repeat("🏴󠁧󠁢󠁳󠁣󠁴󠁿é", 40) // long runs with no spaces, multi-byte runes
	for _, p := range Split(s, 50) {
		if len(p) > 50 {
			t.Errorf("part of %d bytes", len(p))
		}
		if !utf8.ValidString(p) {
			t.Errorf("invalid UTF-8 part %q", p)
		}
	}
}

func TestSplitCapsThePartCount(t *testing.T) {
	got := Split(strings.Repeat("word ", 5000), 40)
	if len(got) != maxParts {
		t.Fatalf("%d parts, want %d", len(got), maxParts)
	}
	if !strings.HasPrefix(got[0], "(1/99) ") || !strings.HasSuffix(got[maxParts-1], "…") {
		t.Errorf("first %q last %q", got[0], got[maxParts-1])
	}
	if got := Split("abcdefghijklmnop", 10); len(got) != 1 || got[0] != "abcdefg…" {
		t.Errorf("tiny budget: %q", got)
	}
}

func TestHelpRepliesFitOnePacket(t *testing.T) {
	// Pinned from the live report: the rc.3 /help reply was 860 bytes.
	long := strings.Repeat("/lxmf on|off|auto — LXMF delivery settings\n", 20)
	for _, p := range Split(long, MaxContent) {
		if len(p) > MaxContent {
			t.Errorf("part of %d bytes", len(p))
		}
	}
}

var partMark = regexp.MustCompile(`^\((\d+)/(\d+)\) `)

// FuzzSplit checks that parts fit, stay valid UTF-8, are numbered in order,
// and together keep every non-space character in order.
func FuzzSplit(f *testing.F) {
	f.Add("Rab: evening all", 40)
	f.Add(strings.Repeat("a b\n\nc ", 80), 64)
	f.Add("é🏴󠁧󠁢󠁳󠁣󠁴󠁿"+strings.Repeat("x", 300), MaxContent)
	f.Fuzz(func(t *testing.T, s string, max int) {
		if max < 16 || max > 4096 || !utf8.ValidString(s) {
			t.Skip()
		}
		parts := Split(s, max)
		var kept strings.Builder
		for i, p := range parts {
			if len(p) > max {
				t.Fatalf("part %d is %d bytes, max %d", i, len(p), max)
			}
			if !utf8.ValidString(p) {
				t.Fatalf("part %d invalid UTF-8", i)
			}
			body := p
			if len(parts) > 1 {
				m := partMark.FindStringSubmatch(p)
				if len(m) != 3 || m[1] != fmt.Sprint(i+1) || m[2] != fmt.Sprint(len(parts)) {
					t.Fatalf("part %d mark: %q", i, p)
				}
				body = p[len(m[0]):]
			}
			kept.WriteString(body)
		}
		if len(parts) >= maxParts || strings.HasSuffix(kept.String(), "…") {
			return // truncated on purpose
		}
		if squeeze(kept.String()) != squeeze(s) {
			t.Fatalf("text changed:\n in %q\nout %q", s, kept.String())
		}
	})
}

func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
