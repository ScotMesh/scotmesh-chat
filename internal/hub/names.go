package hub

import (
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Name rules (ADR 0005).
const (
	NameMinRunes = 2
	NameMaxRunes = 24
	NameMaxBytes = 32 // the RRC K_NICK limit clients enforce
)

// reserved names cannot be claimed by anyone: they would impersonate the
// service or collide with words the chat uses. Compared by skeleton.
var reserved = []string{
	"admin", "administrator", "bridge", "everyone", "guest", "here", "hub",
	"moderator", "nobody", "notice", "op", "operator", "root", "rrc", "scotmesh",
	"server", "system", "you",
}

// NameError explains why a name cannot be used, in words for the person.
type NameError struct{ Reason string }

func (e *NameError) Error() string { return e.Reason }

// CleanName checks a name someone asked for and returns it tidied (NFC,
// trimmed) with its skeleton. The rules:
//
//   - 2 to 24 characters and at most 32 bytes;
//   - starts with a letter;
//   - letters from the Latin alphabet (accents allowed), digits, '-', '_', '.';
//     no spaces, so a name survives @mentions and command arguments;
//   - not a reserved word and not "guest-…".
func CleanName(name string) (clean, skeleton string, err error) {
	n := norm.NFC.String(strings.TrimSpace(name))
	runes := utf8.RuneCountInString(n)
	switch {
	case n == "":
		return "", "", &NameError{"a name can't be empty"}
	case runes < NameMinRunes:
		return "", "", &NameError{"a name needs at least 2 characters"}
	case runes > NameMaxRunes || len(n) > NameMaxBytes:
		return "", "", &NameError{"a name can be at most 24 characters"}
	}
	for i, r := range n {
		if i == 0 && !isPlainLatinLetter(r) {
			return "", "", &NameError{"a name has to start with a letter"}
		}
		allowed := isPlainLatinLetter(r) || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !allowed {
			if unicode.IsSpace(r) {
				return "", "", &NameError{"a name can't contain spaces"}
			}
			return "", "", &NameError{"a name can use letters, digits, '-', '_' and '.' only"}
		}
	}
	sk := Skeleton(n)
	if strings.HasPrefix(sk, "guest") {
		return "", "", &NameError{"names starting with \"guest\" are kept for people without a name"}
	}
	for _, w := range reserved {
		if sk == Skeleton(w) {
			return "", "", &NameError{"\"" + n + "\" is reserved"}
		}
	}
	return n, sk, nil
}

func isLatinLetter(r rune) bool {
	return unicode.Is(unicode.Latin, r) && unicode.IsLetter(r)
}

// isPlainLatinLetter is the character rule a name is checked against: not
// just "some Latin-script letter" (isLatinLetter, above), but one whose
// base, once accents are stripped, is a plain ASCII a-z. Unicode's Latin
// script is far bigger than the alphabet: IPA and click letters like "ɑ"
// (LATIN SMALL LETTER ALPHA), "ʟ" (LATIN LETTER SMALL CAPITAL L), "ı"
// (LATIN SMALL LETTER DOTLESS I) and "ǀ" (LATIN LETTER DENTAL CLICK) all
// pass isLatinLetter and read as ordinary letters in most fonts, but don't
// decompose to an ASCII base, so Skeleton's lookalike folding never sees
// them and "Aǀex" claims a separate name from "Alex". ß is the one
// exception: it has no decomposition either, but Skeleton already folds it
// to "ss", so allowing it doesn't reopen the gap this closes.
func isPlainLatinLetter(r rune) bool {
	if r == 'ß' {
		return true
	}
	var base rune
	found := false
	for _, d := range norm.NFKD.String(string(r)) {
		if unicode.Is(unicode.Mn, d) { // an accent NFKD split off; the base still decides
			continue
		}
		if found { // more than one base rune: not a simple accented letter
			return false
		}
		base, found = d, true
	}
	return found && (base >= 'a' && base <= 'z' || base >= 'A' && base <= 'Z')
}

// Skeleton folds a name to the form used to decide whether two names are
// the same: accents removed, case folded, and characters that look alike in
// common fonts made equal (1 I l | → l, 0 → o, rn → m, vv → w), with '-', '_'
// and '.' dropped. "A1ex", "alex" and "Al.ex" share a skeleton.
func Skeleton(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(name) {
		if unicode.Is(unicode.Mn, r) { // combining marks: the accents NFKD split off
			continue
		}
		switch r = unicode.ToLower(r); r {
		case '-', '_', '.', ' ':
			continue
		case '1', 'i', '|', '!':
			r = 'l'
		case '0':
			r = 'o'
		case '5':
			r = 's'
		case 'ß':
			b.WriteString("ss")
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	s = strings.ReplaceAll(s, "rn", "m")
	s = strings.ReplaceAll(s, "vv", "w")
	return s
}

// GuestName is what someone without a claimed name is called: "guest-" and
// the first four hex digits of their identity hash.
func GuestName(identity []byte) string {
	h := hex.EncodeToString(identity)
	if len(h) > 4 {
		h = h[:4]
	}
	return "guest-" + h
}

// SuggestName turns a display name from somewhere else (an LXMF announce, an
// old nick) into a name the hub would accept, or "" if nothing usable is
// left: spaces become '_', characters a name can't use are dropped, leading
// non-letters and trailing separators are trimmed, and it is cut to length.
// "Casey 🐈" becomes "Casey"; "Ellen Smith" becomes "Ellen_Smith".
func SuggestName(raw string) string {
	var b strings.Builder
	for _, r := range norm.NFC.String(strings.Join(strings.Fields(raw), "_")) {
		switch {
		case isLatinLetter(r), r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	s := strings.TrimLeftFunc(b.String(), func(r rune) bool { return !isLatinLetter(r) })
	for utf8.RuneCountInString(s) > NameMaxRunes || len(s) > NameMaxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	s = strings.TrimRight(s, "-_.")
	clean, _, err := CleanName(s)
	if err != nil {
		return ""
	}
	return clean
}
