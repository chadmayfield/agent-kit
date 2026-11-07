package adapter

import "testing"

func TestStripControl(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "hello\tworld\n", "hello\tworld\n"},
		{"utf8", "café → ok\n", "café → ok\n"},
		{"csi color", "a\x1b[31mred\x1b[0m b", "ared b"},
		{"csi clear", "x\x1b[2J\x1b[1;1Hy", "xy"},
		{"osc title bel", "a\x1b]0;evil title\x07b", "ab"},
		{"osc clipboard st", "a\x1b]52;c;ZXZpbA==\x1b\\b", "ab"},
		{"osc unterminated", "a\x1b]0;rest", "a0;rest"},
		{"dcs", "a\x1bPq#0\x1b\\b", "ab"},
		{"two byte escape", "a\x1bcb", "ab"},
		{"charset escape", "a\x1b(Bb", "ab"},
		{"lone esc at end", "a\x1b", "a"},
		{"c0", "a\rb\x08c\x00d\x07e", "abcde"},
		{"del", "a\x7fb", "ab"},
		{"c1 csi", "a\u009b31mb", "ab"},
		{"c1 osc", "a\u009d0;t\u009cb", "ab"},
		{"other c1", "a\u0085b", "ab"},
		{"invalid utf8", "a\x9b31mb\xff", "a31mb"},
		{"line separators", "a b c", "abc"},
	}
	for _, c := range cases {
		if got := StripControl(c.in); got != c.want {
			t.Errorf("%s: StripControl(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
