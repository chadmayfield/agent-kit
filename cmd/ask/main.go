// Command ask sends one prompt to a coding agent CLI and prints its final
// message.
package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chadmayfield/agent-kit/internal/adapter"
	"github.com/chadmayfield/agent-kit/internal/cli"
)

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	d := cli.Deps{
		Stdin:            os.Stdin,
		StdinIsTerminal:  isTerminal(os.Stdin),
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		StdoutIsTerminal: isTerminal(os.Stdout),
		StderrIsTerminal: isTerminal(os.Stderr),
		Env:              os.Getenv,
		Now:              time.Now,
		Pid:              os.Getpid(),
		Adapters:         adapter.Default(),
		Signals:          sigs,
		Version:          binaryVersion,
		Argv0:            os.Args[0],
		Environ:          os.Environ,
	}
	os.Exit(cli.Execute(cli.NewRootCmd(d)))
}

// binaryVersion runs `<binary> --version` with no stdin, a short timeout
// and only the base environment.
func binaryVersion(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = cli.BaseEnv(os.Environ())
	cmd.WaitDelay = 2 * time.Second
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
