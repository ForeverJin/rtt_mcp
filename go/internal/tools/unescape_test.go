package tools

import "testing"

func TestUnescape(t *testing.T) {
	cases := []struct {
		in   string
		want []byte
	}{
		{"hello", []byte("hello")},                       // no escapes → unchanged
		{"AT\\r\\n", []byte{'A', 'T', '\r', '\n'}},        // the AT-command case: 4 bytes
		{"\\t\\0", []byte{'\t', 0}},                       // tab + NUL
		{"C:\\\\tmp", []byte("C:\\tmp")},                  // \\ → one backslash
		{"\\x0D\\x0A", []byte{'\r', '\n'}},                // hex CR LF
		{"\\x1b[A", []byte{0x1b, '[', 'A'}},               // ESC then plain bytes
		{"\\x7f", []byte{0x7f}},                           // single hex byte
		{"", []byte{}},                                    // empty
		{"plain \\n line", []byte("plain \n line")},       // escape mixed with text
	}
	for _, c := range cases {
		got, err := unescape(c.in)
		if err != nil {
			t.Errorf("unescape(%q) unexpected error: %v", c.in, err)
			continue
		}
		if string(got) != string(c.want) {
			t.Errorf("unescape(%q) = %v (len %d), want %v (len %d)",
				c.in, got, len(got), c.want, len(c.want))
		}
	}
}

func TestUnescapeErrors(t *testing.T) {
	bad := []string{
		"trailing\\", // dangling backslash
		"\\x0",       // short hex (one digit)
		"\\x",        // short hex (no digits)
		"\\xZZ",      // invalid hex digits
		"\\d",        // unknown escape
		"end\\",      // dangling at end
	}
	for _, in := range bad {
		if _, err := unescape(in); err == nil {
			t.Errorf("unescape(%q) expected error, got nil", in)
		}
	}
}
