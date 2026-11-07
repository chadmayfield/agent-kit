package adapter

import "slices"

// extra holds adapters that only some builds add, such as the fake adapter
// in the asktest build.
var extra []Adapter

// Default returns every adapter ask knows, keyed by tool name.
func Default() map[string]Adapter {
	all := []Adapter{
		Codex{},
		NewClaude(),
		NewAgy(),
		NewOpencode(),
	}
	all = append(all, extra...)
	m := make(map[string]Adapter, len(all))
	for _, a := range all {
		m[a.Name()] = a
	}
	return m
}

// CheckEffort refuses an effort the adapter does not list. An empty effort
// always passes.
func CheckEffort(a Adapter, effort string) error {
	if effort == "" || slices.Contains(a.Efforts(), effort) {
		return nil
	}
	return Refuse("effort not supported by %s", a.Name())
}

// SetupError means ask's own setup failed, for example the CLI binary was
// not found. ask exits 3 on it.
type SetupError struct{ Err error }

func (e *SetupError) Error() string {
	if e.Err == nil {
		return "setup failed"
	}
	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *SetupError) Unwrap() error { return e.Err }
