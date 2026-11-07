//go:build !darwin && !linux

package main

import "os"

// isTerminal always reports false on platforms ask does not support.
func isTerminal(*os.File) bool { return false }
