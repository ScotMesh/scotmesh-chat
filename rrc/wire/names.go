package wire

// Derived from rrcd util.py and service.py (_norm_room), Copyright (c) 2025
// S. Miller, KC1AWV, MIT License.

import (
	"strings"
	"unicode/utf8"
)

// DefaultMaxNickBytes is rrcd's default nickname limit, in UTF-8 bytes.
const DefaultMaxNickBytes = 32

// NormalizeNick trims a nickname and reports whether it is usable: non-empty,
// valid UTF-8, at most maxBytes bytes (0 or less means no limit), and free
// of CR, LF and NUL. This is rrcd's normalize_nick. The hub's naming rules
// are stricter and live with the name registry; this is only the wire-level
// check applied to K_NICK.
func NormalizeNick(nick string, maxBytes int) (string, bool) {
	s := strings.TrimSpace(nick)
	if s == "" || !utf8.ValidString(s) {
		return "", false
	}
	if maxBytes > 0 && len(s) > maxBytes {
		return "", false
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", false
	}
	return s, true
}

// NormalizeRoom trims, lowercases and checks a room name.
//
// rrcd accepts any non-empty name within the byte limit. We also strip a
// leading '#' (clients differ on whether they send it) and allow only ASCII
// letters, digits, '-', '_' and '.': room names appear in NomadNet page
// paths, Micron links and log lines, where anything else needs escaping and
// invites lookalikes. Error texts for the checks rrcd has match rrcd's.
func NormalizeRoom(room string, maxBytes int) (string, error) {
	r := strings.ToLower(strings.TrimSpace(room))
	r = strings.TrimPrefix(r, "#")
	if r == "" {
		return "", invalid("room name must not be empty")
	}
	if maxBytes > 0 && len(r) > maxBytes {
		return "", invalid("room name too long: %d bytes > %d bytes", len(r), maxBytes)
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		allowed := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !allowed {
			return "", invalid("room name may only use letters, digits, '-', '_' and '.'")
		}
	}
	return r, nil
}
