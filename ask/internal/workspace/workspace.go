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
// tool cannot write through the copy to files outside it. FIFOs, sockets
// and devices are skipped. Over a limit, Copy removes what it made
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
			if !linkStaysInside(p, link, srcReal) {
				// A tool writing through it would change files outside
				// the copy, which the fingerprint cannot see.
				return nil
			}
			return os.Symlink(link, target)
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

// linkStaysInside reports whether the symlink at p, with target link, is
// safe to copy: the target is relative, and the link resolves, through
// every link on the way, to a path inside srcReal. A relative link that
// resolves inside the source resolves to the same place inside the copy.
// Absolute links, dangling links and links that leave the folder fail.
func linkStaysInside(p, link, srcReal string) bool {
	if filepath.IsAbs(link) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(srcReal, resolved)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
