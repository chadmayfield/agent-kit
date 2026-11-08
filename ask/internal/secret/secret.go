// Package secret looks up the values that config.SecretRef points to.
// Errors name only the secret's config key, never the ref, the value, a
// Keychain item name, a file path, a command or its output.
package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
)

// securityPath is the macOS security tool. Tests point it at a fake.
var securityPath = "/usr/bin/security"

// lookupTimeout bounds each cmd: and keychain: lookup. Tests shorten it.
var lookupTimeout = 30 * time.Second

// maxSecretBytes is the most a file or command may return.
const maxSecretBytes = 64 << 10

// errTooLarge is returned when a lookup returns more than maxSecretBytes.
var errTooLarge = errors.New("lookup returned too much data")

// waitDelay is how long a lookup waits for its output pipes to close after
// the command is killed or exits.
const waitDelay = 2 * time.Second

var (
	errTimeout  = errors.New("lookup timed out")
	errLeftover = errors.New("command left a process holding its output")
)

// Resolve returns the value ref points to, with one trailing "\r\n" or
// "\n" removed. An empty value is an error.
func Resolve(ref config.SecretRef) (string, error) {
	scheme, rest, ok := config.SplitRef(ref.Ref)
	if !ok {
		return "", fail(ref, "ref must start with keychain:, env:, file: or cmd: and name something")
	}

	var (
		val string
		err error
	)
	switch scheme {
	case "env":
		v, set := os.LookupEnv(rest)
		if !set {
			err = errors.New("environment variable is not set")
		}
		val = v
	case "file":
		val, err = readFile(rest)
	case "cmd":
		val, err = runCmd(rest)
	case "keychain":
		account, service := "", rest
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			account, service = rest[:i], rest[i+1:]
		}
		if service == "" {
			err = errors.New("keychain ref has no service")
		} else {
			val, err = keychain(account, service)
		}
	}
	if err != nil {
		return "", fail(ref, err.Error())
	}

	if v, ok := strings.CutSuffix(val, "\r\n"); ok {
		val = v
	} else {
		val = strings.TrimSuffix(val, "\n")
	}
	if val == "" {
		return "", fail(ref, "lookup returned an empty value")
	}
	return val, nil
}

func fail(ref config.SecretRef, why string) error {
	return fmt.Errorf("secret %q: %s", ref.Name, why)
}

// readFile reads path, expanding a leading ~/ to the home folder. Its
// errors leave out the path.
func readFile(path string) (string, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("cannot find the home folder")
		}
		path = filepath.Join(home, rest)
	}
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return "", errors.New("file is not a regular file")
	}
	data, err := readLimited(path)
	switch {
	case errors.Is(err, errTooLarge):
		return "", errTooLarge
	case errors.Is(err, os.ErrNotExist):
		return "", errors.New("file does not exist")
	case errors.Is(err, os.ErrPermission):
		return "", errors.New("file is not readable")
	case err != nil:
		return "", errors.New("file read failed")
	}
	return string(data), nil
}

// readLimited reads the file at path, up to maxSecretBytes.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSecretBytes {
		return nil, errTooLarge
	}
	return data, nil
}

// limitedWriter keeps up to max bytes and fails once a write goes past that.
type limitedWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.over = true
		return 0, errTooLarge
	}
	return w.buf.Write(p)
}

// runCmd runs command with /bin/sh -c in its own process group and returns
// its stdout. Its errors leave out the command and its output.
func runCmd(command string) (string, error) {
	return run("command", true, "/bin/sh", "-c", command)
}

// run runs a lookup command with lookupTimeout and returns its stdout,
// discarding stderr. With group set, the command runs in its own process
// group, which is killed on timeout and when the lookup ends.
func run(what string, group bool, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = waitDelay
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	var stdout limitedWriter
	stdout.max = maxSecretBytes
	cmd.Stdout = &stdout
	err := cmd.Run()
	out := stdout.buf.Bytes()
	if group && cmd.Process != nil {
		// The group id stays reserved while any member lives, so this
		// reaches only leftover children. ESRCH means none are left.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "", errTimeout
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command exited 0, but a child it left kept stdout open
		// until waitDelay. Keep what the command wrote before it exited.
		if len(out) == 0 {
			return "", errLeftover
		}
		err = nil
	}
	if stdout.over {
		return "", errTooLarge
	}
	if err != nil {
		return "", commandError(what, err)
	}
	return string(out), nil
}

// commandError describes a failed lookup command without its arguments or
// output.
func commandError(what string, err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%s exited with status %d", what, exit.ExitCode())
	}
	return fmt.Errorf("%s could not start", what)
}
