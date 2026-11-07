// Package secret looks up the values that config.SecretRef points to.
// Errors name only the secret's config key, never the ref, the value, a
// Keychain item name, a file path, a command or its output.
package secret

import (
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

	"github.com/chadmayfield/agent-kit/internal/config"
)

// securityPath is the macOS security tool. Tests point it at a fake.
var securityPath = "/usr/bin/security"

// lookupTimeout bounds each cmd: and keychain: lookup. Tests shorten it.
var lookupTimeout = 30 * time.Second

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
	scheme, rest, ok := strings.Cut(ref.Ref, ":")
	if !ok || rest == "" {
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
	default:
		err = errors.New("ref must start with keychain:, env:, file: or cmd:")
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
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", errors.New("file does not exist")
	case errors.Is(err, os.ErrPermission):
		return "", errors.New("file is not readable")
	case err != nil:
		return "", errors.New("file read failed")
	}
	return string(data), nil
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
	out, err := cmd.Output()
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
