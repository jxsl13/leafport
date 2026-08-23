package machoutil

import (
	"bytes"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
)

// BinaryFingerprint identifies the executable code of one arm64 Mach-O.
type BinaryFingerprint struct {
	UUID       string
	TextSHA256 string
}

// FingerprintARM64 returns stable identifiers for the arm64 slice. The text
// hash is unaffected by load-command rewriting and ad-hoc code signing.
func FingerprintARM64(data []byte) (BinaryFingerprint, error) {
	thin, err := ThinARM64(data)
	if err != nil {
		return BinaryFingerprint{}, err
	}
	file, err := macho.NewFile(bytes.NewReader(thin))
	if err != nil {
		return BinaryFingerprint{}, fmt.Errorf("parse arm64 Mach-O fingerprint: %w", err)
	}
	defer file.Close()
	var section *macho.Section
	for _, candidate := range file.Sections {
		if candidate.Seg == "__TEXT" && candidate.Name == "__text" {
			section = candidate
			break
		}
	}
	if section == nil {
		return BinaryFingerprint{}, errors.New("arm64 Mach-O has no __TEXT,__text section")
	}
	text, err := section.Data()
	if err != nil {
		return BinaryFingerprint{}, fmt.Errorf("read arm64 __text section: %w", err)
	}
	digest := sha256.Sum256(text)
	fingerprint := BinaryFingerprint{TextSHA256: hex.EncodeToString(digest[:])}
	fingerprint.UUID = machOUUID(file)
	return fingerprint, nil
}

func machOUUID(file *macho.File) string {
	const uuidCommand = 0x1b
	for _, load := range file.Loads {
		raw := load.Raw()
		if len(raw) < 24 || file.ByteOrder.Uint32(raw[:4]) != uuidCommand {
			continue
		}
		uuid := raw[8:24]
		return fmt.Sprintf("%02X%02X%02X%02X-%02X%02X-%02X%02X-%02X%02X-%02X%02X%02X%02X%02X%02X",
			uuid[0], uuid[1], uuid[2], uuid[3], uuid[4], uuid[5], uuid[6], uuid[7],
			uuid[8], uuid[9], uuid[10], uuid[11], uuid[12], uuid[13], uuid[14], uuid[15])
	}
	return ""
}

func (fingerprint BinaryFingerprint) String() string {
	if fingerprint.UUID == "" {
		return "__text SHA-256 " + fingerprint.TextSHA256
	}
	return "UUID " + fingerprint.UUID + ", __text SHA-256 " + fingerprint.TextSHA256
}
