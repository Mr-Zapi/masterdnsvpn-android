// ==============================================================================
// MasterDnsVPN - Android mobile binding tests
// ==============================================================================

package mobile

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"masterdnsvpn-go/internal/config"
)

// TestMobileAndScannerOverrideFieldsAreValid guards against the override map
// keys drifting from the config struct field names (reflect.FieldByName is
// case-sensitive), which silently breaks both Start and TestResolvers at
// runtime.
func TestMobileAndScannerOverrideFieldsAreValid(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte(
		`{"DOMAINS":["test.example.com"],"DATA_ENCRYPTION_METHOD":0,"ENCRYPTION_KEY":"mobile-test-key"}`,
	))

	resolversPath := filepath.Join(t.TempDir(), "resolvers.txt")
	if err := os.WriteFile(resolversPath, []byte("127.0.0.1:53\n"), 0o600); err != nil {
		t.Fatalf("write resolvers: %v", err)
	}

	values := map[string]any{}
	for key, value := range mobileSpeedOverrides() {
		values[key] = value
	}
	// Scanner-only overrides (see TestResolvers in scanner.go).
	values["ProtocolType"] = "SOCKS5"
	values["LocalDNSEnabled"] = false
	values["LogLevel"] = "ERROR"
	values["MTUTestRetries"] = 1
	values["MTUTestTimeout"] = 2.0
	values["SaveMTUServersToFile"] = false

	if _, err := config.LoadClientConfigFromJSONBase64WithOverrides(payload, config.ClientConfigOverrides{
		ResolversFilePath: &resolversPath,
		Values:            values,
	}); err != nil {
		t.Fatalf("mobile/scanner config overrides rejected: %v", err)
	}
}
