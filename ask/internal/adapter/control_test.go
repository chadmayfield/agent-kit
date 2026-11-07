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
		{"osc unterminated at end", "a\x1b]0;rest", "a"},
		{"osc unterminated keeps later lines", "a\x1b]0;title\nline two\nline three\n", "a\nline two\nline three\n"},
		{"osc stops at line end before a later bel", "a\x1b]0;t\nb\x07c", "a\nbc"},
		{"osc body with multibyte text", "a\x1b]0;\u041c\u041c\x07b", "ab"},
		{"dcs", "a\x1bPq#0\x1b\\b", "ab"},
		{"two byte escape", "a\x1bcb", "ab"},
		{"charset escape", "a\x1b(Bb", "ab"},
		{"lone esc at end", "a\x1b", "a"},
		{"c0", "a\rb\x08c\x00d\x07e", "abcde"},
		{"del", "a\x7fb", "ab"},
		{"c1 csi", "a\u009b31mb", "ab"},
		{"c1 osc", "a\u009d0;t\u009cb", "ab"},
		{"other c1", "a\u0085b", "ab"},
		{"raw c1 csi", "a\x9b31mb", "ab"},
		{"raw c1 osc", "a\x9d52;c;ZXZpbA==\x07b", "ab"},
		{"raw c1 osc raw st", "a\x9d0;t\x9cb", "ab"},
		{"raw c1 dcs unterminated", "a\x90q#0\nb", "a\nb"},
		{"raw st alone", "a\x9cb", "ab"},
		{"invalid utf8", "a\xffb\xc3", "ab"},
		{"line separators", "a b c", "abc"},
	}
	for _, c := range cases {
		if got := StripControl(c.in); got != c.want {
			t.Errorf("%s: StripControl(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
