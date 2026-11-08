// Package config loads ask's per-tool settings from a JSON file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config is the whole config file. It never holds a secret value.
type Config struct {
	Tools      map[string]ToolConfig `json:"tools"`
	ReportDays int                   `json:"report_days"`
}

// ToolConfig is the configuration for one CLI. It never holds a secret
// value, only references to where a secret can be found.
type ToolConfig struct {
	Binary          string            `json:"binary"`
	Models          map[string]string `json:"models"`
	TimeoutSeconds  int               `json:"timeout_seconds"`
	Auth            string            `json:"auth"`
	Secrets         []SecretRef       `json:"secrets"`
	ReadOnlySecrets []string          `json:"read_only_secrets"`
	PassEnv         []string          `json:"pass_env"`
}

// SecretRef names a secret, the environment variable the CLI reads it from,
// and where to look it up (keychain:, env:, file: or cmd:).
type SecretRef struct {
	Name   string `json:"name"`
	EnvVar string `json:"env_var"`
	Ref    string `json:"ref"`
}

// schemes are the prefixes a SecretRef.Ref may start with.
var schemes = []string{"keychain:", "env:", "file:", "cmd:"}

// SplitRef splits ref into its scheme (without the colon) and the text after
// it. ok is false when the scheme is unknown or nothing follows it.
func SplitRef(ref string) (scheme, rest string, ok bool) {
	for _, s := range schemes {
		if after, found := strings.CutPrefix(ref, s); found {
			return strings.TrimSuffix(s, ":"), after, after != ""
		}
	}
	return "", "", false
}

// DefaultPath returns $XDG_CONFIG_HOME/agent-kit/config.json, or
// $HOME/.config/agent-kit/config.json when XDG_CONFIG_HOME is unset or not
// absolute. It returns "" when neither is absolute. env looks up an
// environment variable.
func DefaultPath(env func(string) string) string {
	base := env("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home := env("HOME")
		if !filepath.IsAbs(home) {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "agent-kit", "config.json")
}

// Load reads the config file at path. A missing file returns an empty
// Config. An empty path, unknown fields and unknown secret schemes are errors.
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("config: no config path; set HOME or XDG_CONFIG_HOME to an absolute path")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Config{}, fmt.Errorf("config: parse %s: extra data after the JSON object", path)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// validate checks what the JSON decoder cannot. Errors name tools and
// secret names, never a ref.
func (c Config) validate() error {
	if c.ReportDays < 0 {
		return errors.New("report_days is negative")
	}
	for tool, tc := range c.Tools {
		if tc.TimeoutSeconds < 0 {
			return fmt.Errorf("tool %q: timeout_seconds is negative", tool)
		}
		names := map[string]bool{}
		envVars := map[string]bool{}
		for i, s := range tc.Secrets {
			if s.Name == "" {
				return fmt.Errorf("tool %q: secret %d has no name", tool, i+1)
			}
			if names[s.Name] {
				return fmt.Errorf("tool %q: secret %q is listed twice", tool, s.Name)
			}
			names[s.Name] = true
			if s.EnvVar == "" {
				return fmt.Errorf("tool %q: secret %q has no env_var", tool, s.Name)
			}
			if envVars[s.EnvVar] {
				return fmt.Errorf("tool %q: two secrets use env_var %q", tool, s.EnvVar)
			}
			envVars[s.EnvVar] = true
			if !validRef(s.Ref) {
				return fmt.Errorf("tool %q: secret %q: ref must start with keychain:, env:, file: or cmd: and name something", tool, s.Name)
			}
		}
		for _, n := range tc.ReadOnlySecrets {
			if !names[n] {
				return fmt.Errorf("tool %q: read_only_secrets names %q, which is not in secrets", tool, n)
			}
		}
	}
	return nil
}

func validRef(ref string) bool {
	_, _, ok := SplitRef(ref)
	return ok
}

// BinaryFor returns the absolute path of the CLI to run for tool.
//
// With no binary in tc, it looks tool up on PATH. A configured bare name
// (no "/") is looked up on PATH too. A configured path with a "/" is made
// absolute against ask's own working folder, never the tool's -C folder.
// BinaryFor does not check that the file exists; errors never name a path.
func BinaryFor(tool string, tc ToolConfig) (string, error) {
	p := tc.Binary
	switch {
	case p == "":
		lp, err := exec.LookPath(tool)
		if err != nil {
			return "", fmt.Errorf("%s: not found on PATH and no binary set in config", tool)
		}
		p = lp
	case !strings.Contains(p, "/"):
		lp, err := exec.LookPath(p)
		if err != nil {
			return "", fmt.Errorf("%s: the configured binary is not found on PATH", tool)
		}
		p = lp
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%s: cannot make the binary path absolute", tool)
	}
	return abs, nil
}
