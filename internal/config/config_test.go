package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/internal/testenv"
)

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRejectsUnknownScheme(t *testing.T) {
	home := testenv.Isolate(t)
	p := writeConfig(t, home, `{
  "tools": {
    "codex": {
      "secrets": [{"name": "api_key", "env_var": "EXAMPLE_API_KEY", "ref": "vault:placeholder-item"}]
    }
  }
}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load accepted an unknown secret scheme")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error %q does not name the secret", err)
	}
	if strings.Contains(err.Error(), "placeholder-item") {
		t.Errorf("error %q contains the ref", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	home := testenv.Isolate(t)
	p := writeConfig(t, home, `{"tools": {"codex": {"timeout": 5}}}`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}

func TestDefaultPathUsesXDG(t *testing.T) {
	home := testenv.Isolate(t)

	got := DefaultPath(os.Getenv)
	want := filepath.Join(home, ".config", "agent-kit", "config.json")
	if got != want {
		t.Errorf("DefaultPath with XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	xdg := filepath.Join(home, "xdg")
	env := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return xdg
		}
		return os.Getenv(k)
	}
	if got, want := DefaultPath(env), filepath.Join(xdg, "agent-kit", "config.json"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}

	noXDG := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return ""
		}
		return os.Getenv(k)
	}
	if got, want := DefaultPath(noXDG), filepath.Join(home, ".config", "agent-kit", "config.json"); got != want {
		t.Errorf("DefaultPath without XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	relXDG := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "relative/path"
		}
		return os.Getenv(k)
	}
	if got, want := DefaultPath(relXDG), filepath.Join(home, ".config", "agent-kit", "config.json"); got != want {
		t.Errorf("DefaultPath with relative XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	// With no absolute base, DefaultPath returns "" and Load refuses it.
	for _, c := range []struct{ xdg, home string }{
		{"", ""},
		{"", "relative/home"},
		{"relative/path", ""},
		{"relative/path", "relative/home"},
	} {
		env := func(k string) string {
			switch k {
			case "XDG_CONFIG_HOME":
				return c.xdg
			case "HOME":
				return c.home
			}
			return ""
		}
		if got := DefaultPath(env); got != "" {
			t.Errorf("DefaultPath(XDG_CONFIG_HOME=%q, HOME=%q) = %q, want \"\"", c.xdg, c.home, got)
		}
	}
	if _, err := Load(""); err == nil {
		t.Error("Load accepted an empty path")
	}
}

func TestLoadExampleConfig(t *testing.T) {
	testenv.Isolate(t)
	cfg, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	for _, tool := range []string{"codex", "claude", "opencode", "agy"} {
		if _, ok := cfg.Tools[tool]; !ok {
			t.Errorf("example config has no %s entry", tool)
		}
	}
	codex := cfg.Tools["codex"]
	if len(codex.Secrets) == 0 || codex.Secrets[0].Name == "" || codex.Secrets[0].EnvVar == "" || codex.Secrets[0].Ref == "" {
		t.Errorf("codex secrets not decoded: %+v", codex.Secrets)
	}
	if codex.TimeoutSeconds <= 0 {
		t.Errorf("codex timeout_seconds not decoded: %d", codex.TimeoutSeconds)
	}
	if cfg.ReportDays <= 0 {
		t.Errorf("report_days not decoded: %d", cfg.ReportDays)
	}
}

func TestMissingConfigUsesLookPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, src)

	cfg, err := Load(DefaultPath(os.Getenv))
	if err != nil {
		t.Fatalf("Load with no config file: %v", err)
	}
	if len(cfg.Tools) != 0 || cfg.ReportDays != 0 {
		t.Errorf("Load with no config file = %+v, want empty", cfg)
	}

	bin, err := BinaryFor("codex", cfg.Tools["codex"])
	if err != nil {
		t.Fatalf("BinaryFor: %v", err)
	}
	if want := filepath.Join(home, "bin", "codex"); bin != want {
		t.Errorf("BinaryFor = %q, want %q", bin, want)
	}

	if _, err := BinaryFor("opencode", cfg.Tools["opencode"]); err == nil {
		t.Error("BinaryFor found a tool that is not on PATH")
	}

	set := ToolConfig{Binary: "/opt/example/bin/codex"}
	if got, err := BinaryFor("codex", set); err != nil || got != set.Binary {
		t.Errorf("BinaryFor with binary set = %q, %v", got, err)
	}
}

func TestBinaryForBareNameUsesLookPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "example-cli")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := testenv.Isolate(t, src)
	bin, err := BinaryFor("codex", ToolConfig{Binary: "example-cli"})
	if err != nil {
		t.Fatalf("BinaryFor: %v", err)
	}
	if want := filepath.Join(home, "bin", "example-cli"); bin != want {
		t.Errorf("BinaryFor = %q, want %q", bin, want)
	}
	if _, err := BinaryFor("codex", ToolConfig{Binary: "no-such-cli"}); err == nil {
		t.Error("BinaryFor found a bare name that is not on PATH")
	} else if strings.Contains(err.Error(), home) {
		t.Errorf("error %q names a path", err)
	}
}

func TestBinaryForRelativePathIsAbsolute(t *testing.T) {
	testenv.Isolate(t)
	dir := t.TempDir()
	t.Chdir(dir)
	bin, err := BinaryFor("codex", ToolConfig{Binary: "sub/codex"})
	if err != nil {
		t.Fatalf("BinaryFor: %v", err)
	}
	want, err := filepath.Abs(filepath.Join("sub", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if bin != want || !filepath.IsAbs(bin) {
		t.Errorf("BinaryFor = %q, want %q", bin, want)
	}
}

func TestLoadRejectsDuplicateEnvVar(t *testing.T) {
	p := writeConfig(t, t.TempDir(), `{"tools": {"codex": {"secrets": [
  {"name": "api_key", "env_var": "EXAMPLE_API_KEY", "ref": "env:EXAMPLE_ONE"},
  {"name": "other_key", "env_var": "EXAMPLE_API_KEY", "ref": "env:EXAMPLE_TWO"}
]}}}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load accepted two secrets with the same env_var")
	}
	if !strings.Contains(err.Error(), "EXAMPLE_API_KEY") || strings.Contains(err.Error(), "EXAMPLE_ONE") {
		t.Errorf("error %q: want the env_var named and no ref", err)
	}
}
