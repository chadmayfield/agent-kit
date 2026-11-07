//go:build !darwin

package secret

import "errors"

// keychain is macOS only.
func keychain(account, service string) (string, error) {
	return "", errors.New("keychain: refs work on macOS only")
}
