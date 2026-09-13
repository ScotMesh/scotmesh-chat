package hub

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	tests := []struct {
		in, clean, err string
	}{
		{"Alex", "Alex", ""},
		{"  Rab ", "Rab", ""},
		{"Seònaid", "Seònaid", ""},
		{"MM7ROQ", "MM7ROQ", ""},
		{"ellen_b.x-2", "ellen_b.x-2", ""},
		{"Seònaid", "Seònaid", ""}, // decomposed input comes out composed
		{"", "", "a name can't be empty"},
		{"A", "", "a name needs at least 2 characters"},
		{strings.Repeat("a", 25), "", "a name can be at most 24 characters"},
		{strings.Repeat("ò", 17), "", "a name can be at most 24 characters"}, // 17 runes, 34 bytes
		{"7of9", "", "a name has to start with a letter"},
		{"_alex", "", "a name has to start with a letter"},
		{"Alex Smith", "", "a name can't contain spaces"},
		{"alex@home", "", "a name can use letters, digits, '-', '_' and '.' only"},
		{"Алекс", "", "a name has to start with a letter"},                    // Cyrillic
		{"Alеx", "", "a name can use letters, digits, '-', '_' and '.' only"}, // Cyrillic е inside
		// These four are all officially "Latin script" letters, so a check
		// that only asked isLatinLetter would accept them as a look-or-feel
		// match for a-z; none decomposes to a plain ASCII base, so they're
		// refused rather than silently claiming a name a real "Alex" or
		// "Alice" could still be told apart from.
		{"Aǀex", "", "a name can use letters, digits, '-', '_' and '.' only"},  // U+01C0 LATIN LETTER DENTAL CLICK
		{"Alıce", "", "a name can use letters, digits, '-', '_' and '.' only"}, // U+0131 LATIN SMALL LETTER DOTLESS I
		{"ɑlex", "", "a name has to start with a letter"},                      // U+0251 LATIN SMALL LETTER ALPHA
		{"Aʟex", "", "a name can use letters, digits, '-', '_' and '.' only"},  // U+029F LATIN LETTER SMALL CAPITAL L
		{"guest-2af3", "", "names starting with \"guest\" are kept for people without a name"},
		{"Gue5t", "", "names starting with \"guest\" are kept for people without a name"},
		{"Admin", "", "\"Admin\" is reserved"},
		{"Scot.Mesh", "", "\"Scot.Mesh\" is reserved"},
		{"hu_b", "", "\"hu_b\" is reserved"},
	}
	for _, tt := range tests {
		clean, sk, err := CleanName(tt.in)
		gotErr := ""
		if err != nil {
			var ne *NameError
			if !errors.As(err, &ne) {
				t.Errorf("CleanName(%q) error is %T", tt.in, err)
			}
			gotErr = err.Error()
		}
		if clean != tt.clean || gotErr != tt.err {
			t.Errorf("CleanName(%q) = %q, %q; want %q, %q", tt.in, clean, gotErr, tt.clean, tt.err)
		}
		if err == nil && sk != Skeleton(clean) {
			t.Errorf("CleanName(%q) skeleton %q != Skeleton %q", tt.in, sk, Skeleton(clean))
		}
	}
}

func TestSkeletonCatchesLookalikes(t *testing.T) {
	same := [][]string{
		{"Alex", "alex", "ALEX", "A1ex", "AIex", "Al.ex", "a_l-e_x", "Alèx"},
		{"Rab", "rab", "R.a.b"},
		{"Moira", "Mo1ra", "M0ira", "Rnoira", "moiRA"},
		{"Willie", "VVillie", "wi11ie", "Wil|ie"},
		{"Ross", "Roß", "Ro5s"},
	}
	for _, group := range same {
		want := Skeleton(group[0])
		for _, n := range group[1:] {
			if got := Skeleton(n); got != want {
				t.Errorf("Skeleton(%q) = %q, want %q (same as %q)", n, got, want, group[0])
			}
		}
	}
	different := [][2]string{{"Alex", "Alec"}, {"Rab", "Rob"}, {"Ellen", "Allen"}}
	for _, p := range different {
		if Skeleton(p[0]) == Skeleton(p[1]) {
			t.Errorf("%q and %q share a skeleton %q", p[0], p[1], Skeleton(p[0]))
		}
	}
}

func TestGuestName(t *testing.T) {
	if got := GuestName([]byte{0x2a, 0xf3, 0x01}); got != "guest-2af3" {
		t.Errorf("GuestName = %q", got)
	}
	if got := GuestName([]byte{0x2a}); got != "guest-2a" {
		t.Errorf("GuestName(short) = %q", got)
	}
}

func FuzzCleanName(f *testing.F) {
	for _, s := range []string{"Alex", "A1ex", "Seònaid", "guest-1", "a b", "́x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		clean, sk, err := CleanName(s)
		if err != nil {
			return
		}
		// A clean name is stable and its skeleton is non-empty.
		again, sk2, err := CleanName(clean)
		if err != nil || again != clean || sk2 != sk || sk == "" {
			t.Fatalf("CleanName not idempotent: %q -> %q (%q) -> %q (%q), %v", s, clean, sk, again, sk2, err)
		}
	})
}

func TestSuggestName(t *testing.T) {
	for in, want := range map[string]string{
		"Casey 🐈️":                   "Casey",
		"Ellen Smith":                "Ellen_Smith",
		"  7 of 9 ":                  "of_9",
		"🐈":                          "",
		"__Rab__":                    "Rab",
		"Seònaid":                    "Seònaid",
		"John (MM7CAN)":              "John_MM7CAN",
		"admin":                      "",
		strings.Repeat("abcdef", 10): "abcdefabcdefabcdefabcdef",
		"Alex Very-Long Name Here!!": "Alex_Very-Long_Name_Here",
	} {
		if got := SuggestName(in); got != want {
			t.Errorf("SuggestName(%q) = %q, want %q", in, got, want)
		}
	}
}
