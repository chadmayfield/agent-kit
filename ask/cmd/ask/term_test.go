package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsTerminalFalseForNonTerminals(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for _, p := range []string{os.DevNull, "/dev/zero", file} {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if isTerminal(f) {
			t.Errorf("%s reads as a terminal", filepath.Base(p))
		}
		f.Close()
	}
	if isTerminal(r) {
		t.Errorf("a pipe reads as a terminal")
	}
}
