package adapter

import "testing"

func TestQuoteInertEscapesBidi(t *testing.T) {
	got := quoteInert("a\u202eb\u2066c\u061c")
	want := "'a\\u202eb\\u2066c\\u061c'"
	if got != want {
		t.Fatalf("quoteInert = %q, want %q", got, want)
	}
}
