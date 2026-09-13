package lxmfgroup

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ScotMesh/scotmesh-chat/internal/hub"
)

// MaxContent is the most text the group puts in one LXMF message: one
// packet's worth (see hub.OnePacketText for why).
const MaxContent = hub.OnePacketText

// partMarkWidth is the room "(12/12) " takes at the start of a part.
const partMarkWidth = len("(99/99) ")

// maxParts bounds how many parts one message is cut into.
const maxParts = 99

// Split cuts text into parts of at most max bytes each, numbered "(1/3) "
// when there is more than one. It cuts at a blank line if it can, then at a
// line break, then at a space, and only then inside a word, never inside a
// UTF-8 character. Text beyond maxParts parts is dropped, with the last part
// ending in "…".
func Split(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	room := max - partMarkWidth
	if room < 8 {
		return []string{truncate(text, max)}
	}
	var chunks []string
	rest := text
	for rest != "" {
		if len(chunks) == maxParts-1 && len(rest) > room {
			chunks = append(chunks, truncate(rest, room))
			break
		}
		chunk, next := cut(rest, room)
		chunks = append(chunks, chunk)
		rest = next
	}
	if len(chunks) == 1 { // only trailing whitespace was over; no need to number it
		return chunks
	}
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = fmt.Sprintf("(%d/%d) %s", i+1, len(chunks), c)
	}
	return out
}

// cut takes the first part of s that fits in room bytes, and returns it with
// the remainder, trimming the whitespace at the cut.
func cut(s string, room int) (chunk, rest string) {
	if len(s) <= room {
		return s, ""
	}
	window := s[:room+1] // a separator just past the edge still counts
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(window, sep); i > 0 {
			chunk = strings.TrimRight(s[:i], " \n")
			if chunk != "" {
				return chunk, strings.TrimLeft(s[i+len(sep):], " \n")
			}
		}
	}
	i := room
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	if i == 0 { // no rune boundary in reach; can't happen with valid room
		i = room
	}
	return s[:i], s[i:]
}

// truncate shortens s to at most max bytes, ending in "…" if it was cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	i := max - len(ellipsis)
	if i < 0 {
		return s[:0]
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + ellipsis
}
