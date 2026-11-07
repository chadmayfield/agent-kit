//go:build darwin

package secret

// keychain reads a generic password from the login Keychain with
// security find-generic-password. Errors leave out the item name.
func keychain(account, service string) (string, error) {
	args := []string{"find-generic-password"}
	if account != "" {
		args = append(args, "-a", account)
	}
	args = append(args, "-s", service, "-w")
	return run("keychain lookup", false, securityPath, args...)
}
