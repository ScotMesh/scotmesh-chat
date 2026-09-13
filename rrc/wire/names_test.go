package wire

import (
	"strings"
	"testing"
)

func TestNormalizeNick(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
		ok   bool
	}{
		{"Alex", 32, "Alex", true},
		{"  Alex  ", 32, "Alex", true},
		{"", 32, "", false},
		{"   ", 32, "", false},
		{"a\nb", 32, "", false},
		{"a\rb", 32, "", false},
		{"a\x00b", 32, "", false},
		{strings.Repeat("x", 32), 32, strings.Repeat("x", 32), true},
		{strings.Repeat("x", 33), 32, "", false},
		{strings.Repeat("x", 33), 0, strings.Repeat("x", 33), true},
		{"Ceòl", 5, "Ceòl", true}, // ò is 2 bytes: 5 bytes total
		{"Ceòl", 4, "", false},    // limits are bytes, not characters
		{"bad\xffutf8", 32, "", false},
	}
	for _, tt := range tests {
		got, ok := NormalizeNick(tt.in, tt.max)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NormalizeNick(%q, %d) = %q, %v; want %q, %v", tt.in, tt.max, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNormalizeRoom(t *testing.T) {
	tests := []struct {
		in, want, err string
	}{
		{"scotmesh", "scotmesh", ""},
		{"  #ScotMesh ", "scotmesh", ""},
		{"meshcore-868", "meshcore-868", ""},
		{"a.b_c", "a.b_c", ""},
		{"", "", "room name must not be empty"},
		{"#", "", "room name must not be empty"},
		{strings.Repeat("r", 65), "", "room name too long: 65 bytes > 64 bytes"},
		{"two words", "", "room name may only use letters, digits, '-', '_' and '.'"},
		{"cafè", "", "room name may only use letters, digits, '-', '_' and '.'"},
	}
	for _, tt := range tests {
		got, err := NormalizeRoom(tt.in, 64)
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
			if !IsInvalid(err) {
				t.Errorf("NormalizeRoom(%q) error is not a validation error", tt.in)
			}
		}
		if got != tt.want || gotErr != tt.err {
			t.Errorf("NormalizeRoom(%q) = %q, %q; want %q, %q", tt.in, got, gotErr, tt.want, tt.err)
		}
	}
}
