// Package workspace makes a private throwaway copy of a folder and
// fingerprints folders, so ask review can run a tool whose read-only
// setup is unverified in a copy and see whether it changed anything.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Limits bound a copy and a fingerprint. MaxBytes counts the bytes of
// regular files and MaxFiles counts every entry below the folder.
type Limits struct {
	MaxBytes int64
	MaxFiles int
}

// ErrTooLarge means a folder passed a limit. Errors that wrap it say which
// limit.
var ErrTooLarge = errors.New("the folder is too large to review in a copy")

// Copy copies src into a new temp folder with mode 0700 and returns the
// copy's path, which ends in src's base name, and a cleanup that removes
// the temp folder. src may be a symlink to a folder; its real path is
// copied.
//
// Regular files and folders keep their permission bits. A symlink is kept
// only when it is relative, not named .git, not inside a git folder, and
// resolves inside src; it is written as the plain relative path to its
// target. FIFOs, sockets and devices are left out.
//
// Git data is reduced to what reading the repository needs: a .git file or
// symlink is left out, hooks folders are left out, every git config is
// rebuilt from an allowlist (see cleanGitConfig), and commondir, gitdir and
// objects/info/alternates are removed. Git folders are found by the name
// .git in any case, or by their contents.
//
// Over a limit, Copy fails with an error wrapping ErrTooLarge. When ctx is
// done, it stops. Either way it removes what it made. Errors name paths
// relative to src.
func Copy(ctx context.Context, src string, limits Limits) (dir string, cleanup func() error, err error) {
	srcReal, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), relErr(err))
	}
	if fi, err := os.Stat(srcReal); err != nil || !fi.IsDir() {
		return "", nil, fmt.Errorf("copy %s: not a folder", filepath.Base(src))
	}
	root, err := os.MkdirTemp("", "ask-review-")
	if err != nil {
		return "", nil, fmt.Errorf("make the copy folder: %w", err)
	}
	remove := func() error { return removeAll(root) }
	defer func() {
		if err != nil {
			_ = remove()
		}
	}()
	if err = os.Chmod(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("make the copy folder: %w", err)
	}
	dst := filepath.Join(root, filepath.Base(src))

	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	var bytes int64
	files := 0
	err = filepath.WalkDir(srcReal, func(p string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcReal, p)
		if rerr != nil {
			return rerr
		}
		if werr != nil {
			return pathErr(rel, werr)
		}
		target := filepath.Join(dst, rel)
		// A .git file points at another repository's git folder (a
		// worktree or submodule); git in the copy would change that
		// repository.
		if isDotGit(d.Name()) && !d.IsDir() {
			return nil
		}
		// Hooks run on git commands, and remotes and branches are git's
		// older files for push destinations; a reviewer needs none of
		// them. Names match in any case, as git finds them on a
		// case-insensitive file system.
		if d.IsDir() && gitDropDirs[strings.ToLower(d.Name())] && isGitDir(filepath.Dir(p)) {
			return fs.SkipDir
		}
		if rel != "." {
			files++
			if files > limits.MaxFiles {
				return fmt.Errorf("%w: more than %d entries", ErrTooLarge, limits.MaxFiles)
			}
		}
		info, err := d.Info()
		if err != nil {
			return pathErr(rel, err)
		}
		switch {
		case d.IsDir():
			// Owner rwx while copying; the real mode is set at the end.
			if err := os.Mkdir(target, 0o700); err != nil {
				return pathErr(rel, err)
			}
			dirs = append(dirs, dirMode{target, info.Mode().Perm()})
		case d.Type()&fs.ModeSymlink != 0:
			// A link inside a git folder could point git at data that
			// is never cleaned.
			if inGitFolder(srcReal, p) {
				return nil
			}
			link, err := os.Readlink(p)
			if err != nil {
				return pathErr(rel, err)
			}
			canon, ok := insideLink(p, link, srcReal, filepath.Dir(rel))
			if !ok {
				// A tool writing through it would change files outside
				// the copy, which the fingerprint cannot see.
				return nil
			}
			return pathErr(rel, os.Symlink(canon, target))
		case d.Type().IsRegular():
			n, err := copyFile(ctx, p, target, info.Mode().Perm(), limits.MaxBytes-bytes)
			bytes += n
			if errors.Is(err, errOverBudget) {
				return fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limits.MaxBytes)
			}
			return pathErr(rel, err)
		}
		// FIFOs, sockets and devices are not copied.
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	if err = cleanGit(ctx, dst); err != nil {
		return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	// Set folder modes deepest first, so a read-only folder does not block
	// setting the ones inside it.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return "", nil, fmt.Errorf("copy %s: %w", filepath.Base(src), relErr(err))
		}
	}
	return dst, remove, nil
}

// removeAll removes root even when folders inside it are read-only: it
// first gives the owner full access to every folder, without following
// symlinks.
func removeAll(root string) error {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove the copy folder %s: %w", root, err)
	}
	return nil
}

var errOverBudget = errors.New("over the byte budget")

// pathErr replaces the absolute path in an *fs.PathError with rel, so an
// error never shows where the folder lives. nil stays nil.
func pathErr(rel string, err error) error {
	if err == nil {
		return nil
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", rel, pe.Err)
	}
	return fmt.Errorf("%s: %w", rel, err)
}

// relErr drops the path from an *fs.PathError.
func relErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// isDotGit reports whether name is .git in any case, as a case-insensitive
// file system such as macOS's default treats it.
func isDotGit(name string) bool {
	return strings.EqualFold(name, ".git")
}

// gitDropDirs are folders left out when they sit directly in a git
// folder: hooks, and the legacy push destinations remotes and branches.
// refs/remotes is deeper and stays.
var gitDropDirs = map[string]bool{"hooks": true, "remotes": true, "branches": true}

// isGitDir reports whether dir itself is a git folder: named .git in any
// case, or holding git's files.
func isGitDir(dir string) bool {
	return isDotGit(filepath.Base(dir)) || looksLikeGitDir(dir)
}

// gitPointerFiles name files inside a git folder that point git at
// another repository or checkout; they are removed from a copy. Names are
// matched in lower case.
var gitPointerFiles = map[string]bool{"commondir": true, "gitdir": true}

// cleanGit cleans every git folder in the copy at dst, at any depth:
// submodules, nested, bare and worktree git folders included. Each config
// and config.worktree is rebuilt by cleanGitConfig. commondir, gitdir and
// objects/info/alternates are removed, since they point git outside the
// copy.
func cleanGit(ctx context.Context, dst string) error {
	// Decide every action before changing anything: removing commondir
	// first would make its folder stop looking like a git folder.
	type action struct {
		path, rel string
		remove    bool
	}
	var actions []action
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		if werr != nil {
			return pathErr(rel, werr)
		}
		if d.IsDir() || !inGitFolder(dst, p) {
			return nil
		}
		// Git reaches these names in any case on a case-insensitive file
		// system, so they are matched in lower case.
		name := strings.ToLower(d.Name())
		switch {
		case gitPointerFiles[name], strings.HasSuffix(strings.ToLower(filepath.ToSlash(rel)), "objects/info/alternates"):
			actions = append(actions, action{p, rel, true})
		case name == "config" || name == "config.worktree":
			// Copy leaves out every symlink inside a git folder, so this
			// is a regular file.
			actions = append(actions, action{p, rel, false})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.remove {
			err = os.Remove(a.path)
		} else {
			err = cleanGitConfig(ctx, a.path)
		}
		if err != nil {
			return pathErr(a.rel, err)
		}
	}
	return nil
}

// inGitFolder reports whether p lies inside a git folder below root: a
// folder named .git in any case, or one that git could take for a git
// folder because it holds HEAD together with objects, refs or commondir
// (a bare repository or a worktree's git folder, say). Entries are checked
// without following symlinks, and of any type.
func inGitFolder(root, p string) bool {
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
		if isGitDir(dir) {
			return true
		}
		if rel == "." {
			return false
		}
	}
}

// looksLikeGitDir reports whether dir holds HEAD and one of objects, refs
// or commondir, of any type, without following symlinks.
func looksLikeGitDir(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, "HEAD")); err != nil {
		return false
	}
	for _, sub := range []string{"objects", "refs", "commondir"} {
		if _, err := os.Lstat(filepath.Join(dir, sub)); err == nil {
			return true
		}
	}
	return false
}

// gitAllowed lists the only git config keys a copy keeps, by section. They
// describe how the repository is stored; none names a path, a URL or a
// command.
var gitAllowed = map[string]map[string]bool{
	"core": {
		"repositoryformatversion": true, "filemode": true, "bare": true, "ignorecase": true,
		"precomposeunicode": true, "symlinks": true, "logallrefupdates": true,
	},
	"extensions": {
		"objectformat": true, "compatobjectformat": true, "refstorage": true, "worktreeconfig": true,
	},
}

// gitPlainValue is the only value shape kept: a word, number or boolean.
var gitPlainValue = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// cleanGitConfig rebuilds the git config at path from gitAllowed. It stops
// when ctx is done: every
// other section, key, include and continuation line is dropped, so no
// setting can point git at another path or URL, or make it run a command.
// It reads "[section] key = value" on one line, subsections, comments and
// backslash continuations the way git does, and keeps a key only when its
// section has no subsection and its value is plain.
func cleanGitConfig(ctx context.Context, path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	type kv struct{ key, value string }
	kept := map[string][]kv{}
	section := ""
	// A line ending in a backslash continues on the next line. Continued
	// values are never kept, so their lines are skipped without joining
	// them, which keeps the work linear.
	continuing := false
	for i, line := range strings.Split(string(data), "\n") {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		ends := strings.HasSuffix(strings.TrimRight(line, " \t\r"), `\`)
		if continuing {
			continuing = ends
			continue
		}
		if ends {
			continuing = true
			continue
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			end := strings.Index(t, "]")
			if end < 0 {
				section = ""
				continue
			}
			name := strings.TrimSpace(t[1:end])
			if strings.ContainsAny(name, " \t\".") {
				// A subsection: never on the allowlist.
				name = "\x00"
			}
			section = strings.ToLower(name)
			t = strings.TrimSpace(t[end+1:])
		}
		if t == "" || t[0] == '#' || t[0] == ';' {
			continue
		}
		key, value, _ := strings.Cut(t, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if j := strings.IndexAny(value, "#;"); j >= 0 {
			value = strings.TrimSpace(value[:j])
		}
		if gitAllowed[section][key] && gitPlainValue.MatchString(value) {
			kept[section] = append(kept[section], kv{key, value})
		}
	}
	var b strings.Builder
	for _, sec := range []string{"core", "extensions"} {
		if len(kept[sec]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "[%s]\n", sec)
		for _, e := range kept[sec] {
			fmt.Fprintf(&b, "\t%s = %s\n", e.key, e.value)
		}
	}
	// The copied file keeps its source mode, which may be read-only:
	// write with owner access, then put the mode back.
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, fi.Mode().Perm())
}

// insideLink decides whether the symlink at p, with target link, may be
// copied, and returns the target to write in the copy. relDir is the
// folder holding the link, relative to the source root. The link must be
// relative and must resolve, through every link on the way, to a path
// inside srcReal. The copy then gets the canonical relative path from the
// link's real folder to that resolved path, never the original text, so
// the link in the copy resolves exactly where the check looked. Absolute
// links, dangling links and links that leave the folder fail.
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
	// The walk visits only real folders, so this is the link's real
	// parent.
	canon, err := filepath.Rel(filepath.Join(srcReal, relDir), resolved)
	if err != nil {
		return "", false
	}
	return canon, true
}

// ctxReader stops a copy or a hash between reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// copyFile copies a regular file and returns the bytes written. It fails
// with errOverBudget once more than budget bytes would be written. The
// source is opened without blocking and without following a symlink, and
// must still be a regular file once open.
func copyFile(ctx context.Context, src, dst string, perm fs.FileMode, budget int64) (int64, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return 0, errors.New("no longer a regular file")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, ctxReader{ctx, io.LimitReader(in, max(budget, 0)+1)})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > budget {
		return n, errOverBudget
	}
	return n, os.Chmod(dst, perm)
}

// Fingerprint maps each path below dir, relative to it, to what it is:
// "file:<perm>:<size>:<sha256>", "dir:<perm>", "link:<target>", or
// "other" for FIFOs, sockets and devices. It never follows a symlink and
// never opens anything but a regular file. It stops with an error wrapping
// ErrTooLarge past limits, and with ctx's error when ctx is done, so a
// huge file cannot hold it up.
func Fingerprint(ctx context.Context, dir string, limits Limits) (map[string]string, error) {
	fp := map[string]string{}
	var bytes int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if werr != nil {
			return pathErr(rel, werr)
		}
		if rel == "." {
			// The root's mode counts too; its type is a folder.
			info, err := d.Info()
			if err != nil {
				return pathErr(rel, err)
			}
			fp["."] = fmt.Sprintf("dir:%o", info.Mode().Perm())
			return nil
		}
		if len(fp) > limits.MaxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrTooLarge, limits.MaxFiles)
		}
		info, err := d.Info()
		if err != nil {
			return pathErr(rel, err)
		}
		switch {
		case d.IsDir():
			fp[rel] = fmt.Sprintf("dir:%o", info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return pathErr(rel, err)
			}
			fp[rel] = "link:" + link
		case d.Type().IsRegular():
			sum, size, err := hashFile(ctx, p, limits.MaxBytes-bytes)
			bytes += size
			if errors.Is(err, errOverBudget) {
				return fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limits.MaxBytes)
			}
			if err != nil {
				return pathErr(rel, err)
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

// hashFile hashes a regular file of at most budget bytes.
func hashFile(ctx context.Context, p string, budget int64) (string, int64, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", 0, errors.New("no longer a regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, io.LimitReader(f, max(budget, 0)+1)})
	if err != nil {
		return "", n, err
	}
	if n > budget {
		return "", n, errOverBudget
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
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
