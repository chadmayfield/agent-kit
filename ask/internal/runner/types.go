// Package runner starts a CLI and collects its output. It builds on Unix
// only; runner_unix.go supplies the process group calls.
//
// The CLI runs in its own session and process group, and Run stops that
// whole group. A process that calls setsid or setpgid itself leaves the
// group, and Run cannot reach it.
package runner

// Invocation is one fully built command. Adapters fill Binary, Args, Stdin,
// Dir and Cleanup; the cli layer adds Env.
type Invocation struct {
	Binary string
	Args   []string
	// Env is the child's whole environment, as KEY=value pairs.
	Env []string
	// Stdin is written to the child's standard input, then closed.
	Stdin string
	Dir   string
	// Cleanup removes anything Build created, such as a temp prompt file.
	// It may be nil, and it runs on every exit path, including signals.
	Cleanup func()
}
