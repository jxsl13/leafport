package readerconfig

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAccountSecretMatchesReaderFingerprint(t *testing.T) {
	root := t.TempDir()
	secret := strings.Repeat("a1", accountSecretLength/2)
	digest := md5.Sum([]byte(secret))
	if err := os.WriteFile(filepath.Join(root, "state.dat"),
		[]byte("prefix:"+strings.Repeat("b", 41)+"\x00"+secret+"\x00suffix"), 0o600); err != nil {
		t.Fatal(err)
	}
	match, report, err := DiscoverAccountSecret(fmt.Sprintf("%x", digest), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if match != secret {
		t.Fatalf("match = %q, want verified secret", match)
	}
	if report.Files != 1 || report.Candidates != 1 || report.Unreadable != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestDiscoverAccountSecretDoesNotAcceptSubstringOrWrongHash(t *testing.T) {
	root := t.TempDir()
	secret := strings.Repeat("ab", accountSecretLength/2)
	digest := md5.Sum([]byte(strings.Repeat("cd", accountSecretLength/2)))
	data := []byte(secret + "0\n" + secret + "\n")
	if err := os.WriteFile(filepath.Join(root, "state.dat"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	match, report, err := DiscoverAccountSecret(fmt.Sprintf("%x", digest), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if match != "" || report.Candidates != 1 {
		t.Fatalf("match = %q, report = %+v", match, report)
	}
}

func TestDiscoverAccountSecretDecodesHexEncodedValue(t *testing.T) {
	root := t.TempDir()
	secret := strings.Repeat("5e", accountSecretLength/2)
	digest := md5.Sum([]byte(secret))
	encoded := fmt.Sprintf("%x", []byte(secret))
	if err := os.WriteFile(filepath.Join(root, "encoded.dat"), []byte(":"+encoded+";"), 0o600); err != nil {
		t.Fatal(err)
	}
	match, report, err := DiscoverAccountSecret(fmt.Sprintf("%x", digest), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if match != secret || report.Candidates != 1 {
		t.Fatalf("match = %q, report = %+v", match, report)
	}
}

func TestDiscoverAccountSecretDecodesBase64Value(t *testing.T) {
	root := t.TempDir()
	secret := strings.Repeat("9c", accountSecretLength/2)
	digest := md5.Sum([]byte(secret))
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	if err := os.WriteFile(filepath.Join(root, "encoded.dat"), []byte(":"+encoded+";"), 0o600); err != nil {
		t.Fatal(err)
	}
	match, report, err := DiscoverAccountSecret(fmt.Sprintf("%x", digest), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if match != secret || report.Candidates != 1 {
		t.Fatalf("match = %q, report = %+v", match, report)
	}
}

func TestDiscoverAccountSecretSkipsPublicationDirectories(t *testing.T) {
	root := t.TempDir()
	secret := strings.Repeat("12", accountSecretLength/2)
	digest := md5.Sum([]byte(secret))
	bookRoot := filepath.Join(root, "eBooks")
	if err := os.MkdirAll(bookRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bookRoot, "book.dat"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	match, report, err := DiscoverAccountSecret(fmt.Sprintf("%x", digest), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if match != "" || report.Files != 0 {
		t.Fatalf("match = %q, report = %+v", match, report)
	}
}
