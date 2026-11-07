//go:build !darwin

package secret

import (
	"strings"
	"testing"

	"github.com/chadmayfield/agent-kit/ask/internal/config"
	"github.com/chadmayfield/agent-kit/ask/internal/testenv"
)

func TestResolveKeychainOnLinuxFails(t *testing.T) {
	testenv.Isolate(t)
	_, err := Resolve(config.SecretRef{Name: "api_key", EnvVar: "EXAMPLE_API_KEY", Ref: "keychain:placeholder-service"})
	if err == nil {
		t.Fatal("keychain: lookup succeeded off macOS")
	}
	if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("error %q does not say keychain: needs macOS", err)
	}
	if strings.Contains(err.Error(), "placeholder-service") {
		t.Errorf("error %q contains the Keychain item name", err)
	}
}
