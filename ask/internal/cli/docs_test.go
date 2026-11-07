package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// docFiles are the user docs that must name every visible flag.
var docFiles = []string{
	filepath.Join("..", "..", "README.md"),
	filepath.Join("..", "..", "docs", "ask.1"),
}

// TestDocsNameEveryFlag fails when a visible flag of the root command, long
// or short, is missing from the guide or the man page. The man page writes
// a dash as \-, so both spellings count there.
func TestDocsNameEveryFlag(t *testing.T) {
	cmd := NewRootCmd(Deps{})
	cmd.InitDefaultHelpFlag()

	var names, shorts []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		names = append(names, f.Name)
		if f.Shorthand != "" {
			shorts = append(shorts, f.Shorthand)
		}
	})
	if len(names) == 0 {
		t.Fatal("the root command defines no flags")
	}

	for _, path := range docFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		for _, name := range names {
			plain := "--" + name
			roff := `\-\-` + strings.ReplaceAll(name, "-", `\-`)
			if !strings.Contains(text, plain) && !strings.Contains(text, roff) {
				t.Errorf("%s does not mention %s", path, plain)
			}
		}
		// A short form counts only as a whole word: `-C` in the guide,
		// \-C followed by a space or a line end in the man page.
		for _, s := range shorts {
			plain := "`-" + s + "`"
			roff := `\-` + s
			if !strings.Contains(text, plain) &&
				!strings.Contains(text, roff+" ") && !strings.Contains(text, roff+"\n") {
				t.Errorf("%s does not mention -%s", path, s)
			}
		}
	}
}
