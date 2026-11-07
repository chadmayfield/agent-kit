// Package record saves ask's report files and appends to its run log.
//
// Reports hold only the final message. The log holds only the fields of
// LogEntry, so the prompt never reaches either.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// maxSuffix bounds the -<n> search.
const maxSuffix = 10000

// timeLayout is UTC time with no colons, safe in any file name.
const timeLayout = "20060102T150405Z"

// SaveReport writes final to a new file in dir and returns its path. The
// name is <time>-<tool>-<pid>-<n>.md, with n counting up from 1 until a
// name is free. The file is created exclusively with mode 0600. dir is
// created with mode 0700, or tightened to 0700 when it exists. A symlinked
// dir, or a symlink at the report path, is refused.
func SaveReport(dir string, now time.Time, tool string, pid int, final string) (string, error) {
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	if !strings.HasSuffix(final, "\n") {
		final += "\n"
	}
	base := fmt.Sprintf("%s-%s-%d", now.UTC().Format(timeLayout), tool, pid)
	for n := 1; n <= maxSuffix; n++ {
		p := filepath.Join(dir, fmt.Sprintf("%s-%d.md", base, n))
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, fs.ErrExist) {
			fi, lerr := os.Lstat(p)
			if lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
				return "", fmt.Errorf("report: %s is a symlink; refusing to write", filepath.Base(p))
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("report: create %s: %w", filepath.Base(p), err)
		}
		// Set the exact mode, whatever the umask.
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("report: set mode: %w", err)
		}
		if _, err := f.WriteString(final); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("report: write: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("report: close: %w", err)
		}
		return p, nil
	}
	return "", fmt.Errorf("report: no free name for %s after %d tries", base, maxSuffix)
}

// ensureDir prepares the report folder dir and the state folder above it.
// Both are refused when they are a symlink or not a folder, and made with
// mode 0700 when missing. dir is also tightened to 0700 when it exists.
// Folders above the state folder are not checked.
func ensureDir(dir string) error {
	if err := prepDir("report", filepath.Dir(dir), false); err != nil {
		return err
	}
	return prepDir("report", dir, true)
}

// prepDir refuses a symlinked or non-folder dir, creates a missing one with
// mode 0700, and with tighten set chmods an existing one to 0700.
func prepDir(what, dir string, tighten bool) error {
	err := checkDir(what, dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("%s: make folder: %w", what, err)
		}
		if err := checkDir(what, dir); err != nil {
			return err
		}
	case err != nil:
		return err
	case !tighten:
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%s: set folder mode: %w", what, err)
	}
	return nil
}

// checkDir returns fs.ErrNotExist when dir is missing, and an error when
// dir is a symlink or not a folder.
func checkDir(what, dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.ErrNotExist
		}
		return fmt.Errorf("%s: check folder: %w", what, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s: %s is a symlink; refusing to use it", what, filepath.Base(dir))
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: %s is not a folder", what, filepath.Base(dir))
	}
	return nil
}

// Prune deletes report files (regular *.md files) in dir last changed more
// than days days before now. days of 0 or less deletes nothing. A missing
// dir is not an error; a symlinked dir is refused.
func Prune(dir string, now time.Time, days int) error {
	if err := checkDir("report", filepath.Dir(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := checkDir("report", dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if days <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("report: read folder: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("report: remove %s: %w", e.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// LogEntry is one line of the run log. It has no field for the prompt or
// the final message.
type LogEntry struct {
	Time  time.Time `json:"time"`
	Tool  string    `json:"tool"`
	Model string    `json:"model"`
	// ModelID is the model string passed to the CLI after the alias lookup.
	ModelID string `json:"model_id"`
	// ReportedModel is the model the CLI named in its own output, or empty.
	ReportedModel string `json:"reported_model"`
	DurationMS    int64  `json:"duration_ms"`
	Exit          int    `json:"exit"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
}

// AppendLog adds e to the log at path as one JSON line. The file is mode
// 0600. Its folder is created with mode 0700, and a symlinked folder or a
// symlink at path is refused.
func AppendLog(path string, e LogEntry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("log: encode: %w", err)
	}
	line = append(line, '\n')
	if err := prepDir("log", filepath.Dir(path), false); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("log: open: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("log: set mode: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("log: write: %w", err)
	}
	return f.Close()
}
