package readerconfig

import (
	"crypto/md5" // #nosec G501 -- compatibility with the reader's stored lookup fingerprint.
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	accountSecretLength       = 40
	hexEncodedSecretLength    = accountSecretLength * 2
	base64SecretLength        = 56
	maximumCredentialFileSize = 64 << 20
)

// AccountSecretDiscovery reports a value-free summary of a local credential
// scan. It is safe to print: paths, candidates, hashes, and secrets are never
// retained in the report.
type AccountSecretDiscovery struct {
	Files      int
	Bytes      int64
	Candidates int
	Unreadable int
}

// DefaultAccountSecretRoots returns the reader-owned, non-publication storage
// locations that can legally be read by the current user. The eBook library is
// excluded by DiscoverAccountSecret because vouchers and content cannot be a
// source of the raw account credential.
func DefaultAccountSecretRoots(home string) []string {
	return []string{
		filepath.Join(home, "Library", "Containers", "com.amazon.Lassen", "Data"),
		filepath.Join(home, "Library", "Containers", "com.amazon.Lassen.SendToKindleExtension", "Data"),
		filepath.Join(home, "Library", "Group Containers", "group.com.amazon.Lassen"),
	}
}

// DiscoverAccountSecret searches reader-owned files for an exact 40-digit
// hexadecimal value, either raw, hex-encoded, or base64-encoded, whose MD5
// fingerprint equals HashedAccountSecret. Reverse
// engineering of AuthenticationManager confirms that this is the reader's
// compatibility fingerprint. Candidate values stay in the scanner and only a
// verified match is returned.
func DiscoverAccountSecret(hashed string, roots []string) (string, AccountSecretDiscovery, error) {
	var report AccountSecretDiscovery
	hashed = strings.TrimSpace(hashed)
	if len(hashed) != md5.Size*2 {
		return "", report, fmt.Errorf("hashed account-secret length is %d; expected %d", len(hashed), md5.Size*2)
	}
	target, err := hex.DecodeString(hashed)
	if err != nil {
		return "", report, errors.New("hashed account secret is not hexadecimal")
	}

	var match string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				report.Unreadable++
				return nil
			}
			if entry.IsDir() {
				if path != root && isPublicationDirectory(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				report.Unreadable++
				return nil
			}
			if info.Size() < accountSecretLength || info.Size() > maximumCredentialFileSize {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				report.Unreadable++
				return nil
			}
			scanner := accountSecretScanner{target: target}
			read, scanErr := io.Copy(&scanner, io.LimitReader(file, maximumCredentialFileSize+1))
			closeErr := file.Close()
			scanner.finish()
			report.Files++
			report.Bytes += read
			report.Candidates += scanner.candidates
			if scanErr != nil || closeErr != nil {
				report.Unreadable++
				return nil
			}
			if scanner.conflict {
				return errors.New("multiple distinct account secrets match the stored fingerprint")
			}
			if scanner.match == "" {
				return nil
			}
			if match != "" && match != scanner.match {
				return errors.New("multiple distinct account secrets match the stored fingerprint")
			}
			match = scanner.match
			return nil
		})
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", report, fmt.Errorf("scan reader credential storage: %w", err)
		}
	}
	return match, report, nil
}

func isPublicationDirectory(name string) bool {
	switch strings.ToLower(name) {
	case "ebooks", "kindle content":
		return true
	default:
		return false
	}
}

type accountSecretScanner struct {
	target         []byte
	hexRun         [hexEncodedSecretLength]byte
	hexLength      int
	hexOverflow    bool
	base64Run      [base64SecretLength]byte
	base64Length   int
	base64Overflow bool
	candidates     int
	match          string
	conflict       bool
}

func (scanner *accountSecretScanner) Write(data []byte) (int, error) {
	for _, value := range data {
		if isHexDigit(value) {
			if scanner.hexLength < len(scanner.hexRun) {
				scanner.hexRun[scanner.hexLength] = value
				scanner.hexLength++
			} else {
				scanner.hexOverflow = true
			}
		} else {
			scanner.finishHexRun()
		}
		if isBase64Digit(value) {
			if scanner.base64Length < len(scanner.base64Run) {
				scanner.base64Run[scanner.base64Length] = value
				scanner.base64Length++
			} else {
				scanner.base64Overflow = true
			}
		} else {
			scanner.finishBase64Run()
		}
	}
	return len(data), nil
}

func (scanner *accountSecretScanner) finish() {
	scanner.finishHexRun()
	scanner.finishBase64Run()
}

func (scanner *accountSecretScanner) finishHexRun() {
	if !scanner.hexOverflow {
		switch scanner.hexLength {
		case accountSecretLength:
			scanner.testCandidate(scanner.hexRun[:accountSecretLength])
		case hexEncodedSecretLength:
			var decoded [accountSecretLength]byte
			if _, err := hex.Decode(decoded[:], scanner.hexRun[:]); err == nil && allHexDigits(decoded[:]) {
				scanner.testCandidate(decoded[:])
			}
			clear(decoded[:])
		}
	}
	clear(scanner.hexRun[:])
	scanner.hexLength = 0
	scanner.hexOverflow = false
}

func (scanner *accountSecretScanner) finishBase64Run() {
	if !scanner.base64Overflow && (scanner.base64Length == 54 || scanner.base64Length == base64SecretLength) {
		encoded := scanner.base64Run[:scanner.base64Length]
		for _, encoding := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
		} {
			var decoded [base64SecretLength]byte
			length, err := encoding.Decode(decoded[:], encoded)
			if err == nil && length == accountSecretLength && allHexDigits(decoded[:length]) {
				scanner.testCandidate(decoded[:length])
				clear(decoded[:])
				break
			}
			clear(decoded[:])
		}
	}
	clear(scanner.base64Run[:])
	scanner.base64Length = 0
	scanner.base64Overflow = false
}

func (scanner *accountSecretScanner) testCandidate(candidate []byte) {
	scanner.candidates++
	digest := md5.Sum(candidate) // #nosec G401 -- matching the reader's compatibility fingerprint.
	if subtle.ConstantTimeCompare(digest[:], scanner.target) != 1 {
		return
	}
	value := string(candidate)
	if scanner.match != "" && scanner.match != value {
		scanner.conflict = true
	} else {
		scanner.match = value
	}
}

func isHexDigit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func isBase64Digit(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value == '+' || value == '/' || value == '-' || value == '_' || value == '='
}

func allHexDigits(values []byte) bool {
	for _, value := range values {
		if !isHexDigit(value) {
			return false
		}
	}
	return true
}
