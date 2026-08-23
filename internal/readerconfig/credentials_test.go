package readerconfig

import (
	"os"
	"path/filepath"
	"testing"

	"howett.net/plist"
)

func TestLoadCredentialsKeepsHashedAccountSecretSeparate(t *testing.T) {
	data, err := plist.Marshal(map[string]any{
		"kindle_notifications_persist_NotificationsCustomData": `{"dsn":"device-123"}`,
		"HashedAccountSecret": "secret-hash",
	}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reader.plist")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.DSN != "device-123" || credentials.HashedAccountSecret != "secret-hash" ||
		len(credentials.AccountSecrets) != 0 {
		t.Fatalf("unexpected credentials: DSN=%q hash=%t secrets=%d", credentials.DSN,
			credentials.HashedAccountSecret != "", len(credentials.AccountSecrets))
	}
}

func TestInspectVouchers(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "book.voucher"),
		[]byte("ProtectedData ACCOUNT_SECRET CLIENT_ID"), 0o600); err != nil {
		t.Fatal(err)
	}
	requirements, err := InspectVouchers(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if requirements.Vouchers != 1 || !requirements.AccountSecret || !requirements.ClientID {
		t.Fatalf("unexpected requirements: %+v", requirements)
	}
}
