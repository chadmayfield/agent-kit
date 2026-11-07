//go:build unix

package runner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// setProcAttr starts the child in a new session, as leader of its own
// process group and with no controlling terminal.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalGroup sends sig to every process in the group pgid.
func signalGroup(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// groupAlive reports whether any process in the group pgid still exists.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signalNumber returns the number of s, or 0 if s is not a Unix signal.
func signalNumber(s os.Signal) int {
	if sig, ok := s.(syscall.Signal); ok {
		return int(sig)
	}
	return 0
}

// exitStatus returns the exit code and the killing signal. The code is -1
// and the signal non-zero when a signal ended the process.
func exitStatus(ps *os.ProcessState) (code, sig int) {
	if ps == nil {
		return -1, 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, int(ws.Signal())
	}
	return ps.ExitCode(), 0
}
