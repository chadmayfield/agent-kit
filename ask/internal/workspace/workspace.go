// Package workspace makes a private throwaway copy of a folder and
// fingerprints folders, so ask review can run a tool that may write in a
// copy and see whether it changed anything.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Limits bound a copy. MaxBytes counts the bytes of regular files and
// MaxFiles counts every entry below the source folder.
type Limits struct {
	MaxBytes int64
	MaxFiles int
}

// Copy copies src into a new temp folder with mode 0700 and returns the
// copy's path, which ends in src's base name, and a cleanup that removes
// the temp folder. Regular files keep their permission bits, folders keep
// theirs, and symlinks are copied as links and never followed. A symlink
// that is absolute, dangling, or resolves outside src is left out, so a
// tool cannot write through the copy to files outside it. A .git file,
// which points at another repository, is left out, and the copy's git
// config loses its remotes, URL rewrites, credential helpers and
// includes. FIFOs, sockets and devices are skipped. Over a limit, Copy removes what it made
// and returns an error naming the limit.
func Copy(src string, limits Limits) (dir string, cleanup func(), err error) {
	root, err := os.MkdirTemp("", "ask-review-")
	if err != nil {
		return "", nil, fmt.Errorf("make the copy folder: %w", err)
	}
	// The error paths return a nil cleanup, so the deferred removal uses
	// its own copy of the function.
	remove := func() { _ = os.RemoveAll(root) }
	defer func() {
		if err != nil {
			remove()
		}
	}()
	if err = os.Chmod(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("make the copy folder: %w", err)
	}
	dst := filepath.Join(root, filepath.Base(src))
	// Links are judged against the source's real path.
	srcReal, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}

	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	var bytes int64
	files := 0
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		// A .git file points at a real repository's git folder (a
		// worktree or submodule); git commands in the copy would change
		// that repository, so the pointer stays behind.
		if d.Name() == ".git" && d.Type().IsRegular() {
			return nil
		}
		if rel != "." {
			files++
			if files > limits.MaxFiles {
				return fmt.Errorf("the folder has more than %d entries, the most ask review copies", limits.MaxFiles)
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			// Owner rwx while copying; the real mode is set at the end.
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, info.Mode().Perm()})
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			canon, ok := insideLink(p, link, srcReal, filepath.Dir(rel))
			if !ok {
				// A tool writing through it would change files outside
				// the copy, which the fingerprint cannot see.
				return nil
			}
			return os.Symlink(canon, target)
		case d.Type().IsRegular():
			n, err := copyFile(p, target, info.Mode().Perm(), limits.MaxBytes-bytes)
			bytes += n
			if errors.Is(err, errTooBig) {
				return fmt.Errorf("the folder holds more than %d bytes, the most ask review copies", limits.MaxBytes)
			}
			return err
		}
		// FIFOs, sockets and devices are not copied.
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	if err = sanitizeGit(dst); err != nil {
		return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	// Set folder modes deepest first, so a read-only folder does not block
	// setting the ones inside it.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
		}
	}
	return dst, remove, nil
}

var errTooBig = errors.New("over the byte limit")

// gitSectionsDropped are git config sections left out of a copy: remotes
// and URL rewrites (so a plain git push has nowhere to go), credential
// helpers, and includes, which could pull in config from outside.
var gitSectionsDropped = map[string]bool{
	"remote": true, "url": true, "credential": true, "include": true, "includeif": true,
}

// gitPointerFiles name files inside a git folder that point git at
// another repository or checkout; they are removed from a copy.
var gitPointerFiles = map[string]bool{"commondir": true, "gitdir": true}

// sanitizeGit cleans every git folder in the copy at dst, at any depth:
// submodules, nested repositories and worktree data included. Each config
// and config.worktree loses the sections in gitSectionsDropped and its
// core.worktree; one that is a symlink becomes a cleaned regular file.
// commondir, gitdir and objects/info/alternates are removed, since they
// point git outside the copy.
func sanitizeGit(dst string) error {
	return filepath.WalkDir(dst, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil || d.IsDir() || !inGitFolder(rel) {
			return err
		}
		name := d.Name()
		switch {
		case gitPointerFiles[name], strings.HasSuffix(filepath.ToSlash(rel), "objects/info/alternates"):
			return os.Remove(p)
		case name == "config" || name == "config.worktree":
			if d.Type()&fs.ModeSymlink != 0 {
				// The link resolves inside the copy (Copy keeps no
				// other kind); replace it with a cleaned file.
				data, err := os.ReadFile(p)
				if err := os.Remove(p); err != nil {
					return err
				}
				if err != nil {
					return nil
				}
				if err := os.WriteFile(p, data, 0o644); err != nil {
					return err
				}
			}
			return stripGitConfig(p)
		}
		return nil
	})
}

// inGitFolder reports whether the relative path rel lies inside a folder
// named .git.
func inGitFolder(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == ".git" {
			return true
		}
	}
	return false
}

// stripGitConfig rewrites the git config at path, when it is a regular
// file, without the sections in gitSectionsDropped and without
// core.worktree. Section names match
// in any case, in both the [remote "x"] and the older [remote.x] forms.
func stripGitConfig(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var kept []string
	drop := false
	section := ""
	for _, line := range strings.SplitAfter(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			name := strings.TrimLeft(t, "[")
			if i := strings.IndexAny(name, " \t\".]"); i >= 0 {
				name = name[:i]
			}
			section = strings.ToLower(name)
			drop = gitSectionsDropped[section]
		} else if section == "core" && gitKey(t) == "worktree" {
			// core.worktree points git at a checkout outside the copy.
			continue
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(kept, "")), fi.Mode().Perm())
}

// insideLink decides whether the symlink at p, with target link, may be
// copied, and returns the target to write in the copy. relDir is the
// folder holding the link, relative to the source root. The link must be
// relative and must resolve, through every link on the way, to a path
// inside srcReal. The copy then gets the canonical relative path from the
// link's real folder to that resolved path, never the original text, so
// the link in the copy resolves exactly where the check looked.
// Absolute links, dangling links and links that leave the folder fail.
func insideLink(p, link, srcReal, relDir string) (string, bool) {
	if filepath.IsAbs(link) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	if rel, err := filepath.Rel(srcReal, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// Folders the walk visits are real folders, so this is the link's
	// real parent.
	canon, err := filepath.Rel(filepath.Join(srcReal, relDir), resolved)
	if err != nil {
		return "", false
	}
	return canon, true
}

// copyFile copies a regular file and returns the bytes written. It fails
// with errTooBig once more than budget bytes would be written. The source
// is opened without blocking and must still be a regular file once open.
func copyFile(src, dst string, perm fs.FileMode, budget int64) (int64, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is no longer a regular file", filepath.Base(src))
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, max(budget, 0)+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > budget {
		return n, errTooBig
	}
	return n, os.Chmod(dst, perm)
}

// Fingerprint maps each path below dir, relative to it, to what it is:
// "file:<perm>:<size>:<sha256>", "dir:<perm>", "link:<target>", or
// "other" for FIFOs, sockets and devices. It never follows a symlink and
// never opens anything but a regular file.
func Fingerprint(dir string) (map[string]string, error) {
	fp := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			fp[rel] = fmt.Sprintf("dir:%o", info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fp[rel] = "link:" + link
		case d.Type().IsRegular():
			sum, size, err := hashFile(p)
			if err != nil {
				return err
			}
			fp[rel] = fmt.Sprintf("file:%o:%d:%s", info.Mode().Perm(), size, sum)
		default:
			fp[rel] = "other"
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fingerprint %s: %w", filepath.Base(dir), err)
	}
	return fp, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is no longer a regular file", filepath.Base(p))
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// gitKey returns the lowercased key of a git config line such as
// "worktree = /x", or "" for anything else.
func gitKey(line string) string {
	if line == "" || line[0] == '#' || line[0] == ';' {
		return ""
	}
	key := line
	if i := strings.IndexAny(key, " \t="); i >= 0 {
		key = key[:i]
	}
	return strings.ToLower(key)
}

// Changed returns the paths, sorted, that were added, removed or differ
// between two fingerprints.
func Changed(before, after map[string]string) []string {
	var out []string
	for p, v := range before {
		if after[p] != v {
			out = append(out, p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
