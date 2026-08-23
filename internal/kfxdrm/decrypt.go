// Package kfxdrm decrypts the DRMION records used in downloaded KFX bundles.
package kfxdrm

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/ulikunitz/xz/lzma"
)

var (
	drmIonPrefix = []byte{0xea, 'D', 'R', 'M', 'I', 'O', 'N', 0xee}
)

// ProtectedData shared symbol table IDs. The system table occupies 1..9.
const (
	sidEnvelopeV1      = 10
	sidMetadataV1      = 11
	sidEncryptionKey   = 14
	sidEncryptedPageV1 = 20
	sidCipherText      = 21
	sidCipherIV        = 22
	sidData            = 24
	sidPlainTextV1     = 41
	sidEndDoc          = 52
	sidEnvelopeV2      = 67
	sidMetadataV2      = 68
	sidEncryptedPageV2 = 69
	sidPlainTextV2     = 70
	sidCompressedV1    = 72
)

// Stats summarizes a standalone KFX export.
type Stats struct {
	Files            int
	EncryptedRecords int
	Pages            int
}

// KeyValidation summarizes the encrypted sample records successfully opened
// before an output archive is created.
type KeyValidation struct {
	Records int
	Pages   int
}

// ValidateBundleKey decrypts up to three independent encrypted records in
// memory. This rejects a wrong or stale captured key before creating the
// destination archive.
func ValidateBundleKey(bundle string, key []byte) (validation KeyValidation, err error) {
	entries, err := os.ReadDir(bundle)
	if err != nil {
		return validation, fmt.Errorf("read bundle: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	const sampleLimit = 3
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return validation, infoErr
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(bundle, entry.Name()))
		if readErr != nil {
			return validation, readErr
		}
		if !isDRMION(data) {
			continue
		}
		values, parseErr := parseIon(data[len(drmIonPrefix) : len(data)-8])
		if parseErr != nil {
			return validation, fmt.Errorf("%s: parse DRMION sample: %w", entry.Name(), parseErr)
		}
		if !containsAnnotated(values, sidEncryptedPageV1, sidEncryptedPageV2) {
			continue
		}
		plaintext, pages, decryptErr := DecryptRecord(data, key)
		clear(plaintext)
		if decryptErr != nil {
			return validation, fmt.Errorf("%s: %w", entry.Name(), decryptErr)
		}
		validation.Records++
		validation.Pages += pages
		if validation.Records == sampleLimit {
			break
		}
	}
	if validation.Records == 0 {
		return validation, errors.New("bundle contains no encrypted DRMION sample record")
	}
	return validation, nil
}

// DecryptRecord converts one complete DRMION record into its KFX payload.
func DecryptRecord(record, key []byte) ([]byte, int, error) {
	if len(key) < aes.BlockSize {
		return nil, 0, fmt.Errorf("content key is %d bytes; need at least %d", len(key), aes.BlockSize)
	}
	const footerLength = 8
	if len(record) < len(drmIonPrefix)+footerLength ||
		!bytes.Equal(record[:len(drmIonPrefix)], drmIonPrefix) {
		return nil, 0, errors.New("not a complete DRMION record")
	}
	values, err := parseIon(record[len(drmIonPrefix) : len(record)-footerLength])
	if err != nil {
		return nil, 0, fmt.Errorf("parse DRMION envelope: %w", err)
	}
	if !containsAnnotated(values, sidEnvelopeV1, sidEnvelopeV2) {
		return nil, 0, errors.New("DRMION envelope annotation is missing")
	}

	var output bytes.Buffer
	pages := 0
	var visit func([]*ionValue) error
	visit = func(items []*ionValue) error {
		for _, item := range items {
			switch {
			case item.hasAnnotation(sidEncryptedPageV1) || item.hasAnnotation(sidEncryptedPageV2):
				page, pageErr := decryptPage(item, key[:aes.BlockSize])
				if pageErr != nil {
					return fmt.Errorf("decrypt page %d: %w", pages+1, pageErr)
				}
				if _, pageErr = output.Write(page); pageErr != nil {
					return pageErr
				}
				pages++
				continue
			case item.hasAnnotation(sidPlainTextV1) || item.hasAnnotation(sidPlainTextV2):
				page, pageErr := plainPage(item)
				if pageErr != nil {
					return fmt.Errorf("read plaintext page %d: %w", pages+1, pageErr)
				}
				if _, pageErr = output.Write(page); pageErr != nil {
					return pageErr
				}
				pages++
				continue
			}
			if err := visit(item.children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(values); err != nil {
		return nil, pages, err
	}
	if pages == 0 {
		return nil, 0, errors.New("DRMION envelope contains no content pages")
	}
	return output.Bytes(), pages, nil
}

func decryptPage(page *ionValue, key []byte) ([]byte, error) {
	var ciphertext, iv []byte
	compressed := false
	for _, field := range page.children {
		if field.hasAnnotation(sidCompressedV1) {
			compressed = true
		}
		switch field.fieldID {
		case sidCipherText:
			ciphertext = field.data
		case sidCipherIV:
			iv = field.data
		}
	}
	if len(iv) < aes.BlockSize {
		return nil, fmt.Errorf("cipher IV is %d bytes", len(iv))
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not block-aligned", len(ciphertext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv[:aes.BlockSize]).CryptBlocks(plaintext, ciphertext)
	plaintext, err = unpadPKCS7(plaintext, aes.BlockSize)
	if err != nil {
		return nil, err
	}
	return decompressPage(plaintext, compressed)
}

func plainPage(page *ionValue) ([]byte, error) {
	compressed := false
	var data []byte
	for _, field := range page.children {
		if field.hasAnnotation(sidCompressedV1) {
			compressed = true
		}
		if field.fieldID == sidData {
			data = field.data
		}
	}
	if data == nil {
		return nil, errors.New("data field is missing")
	}
	return decompressPage(data, compressed)
}

func decompressPage(data []byte, compressed bool) ([]byte, error) {
	if !compressed {
		return data, nil
	}
	if len(data) < 2 || data[0] != 0 {
		return nil, errors.New("unsupported DRMION LZMA filter")
	}
	reader, err := lzma.NewReader(bytes.NewReader(data[1:]))
	if err != nil {
		return nil, fmt.Errorf("open LZMA page: %w", err)
	}
	result, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decompress LZMA page: %w", err)
	}
	return result, nil
}

func unpadPKCS7(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("invalid padded plaintext length")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, errors.New("invalid PKCS#7 padding (wrong content key)")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid PKCS#7 padding (wrong content key)")
		}
	}
	return data[:len(data)-padding], nil
}

func containsAnnotated(values []*ionValue, ids ...uint64) bool {
	for _, value := range values {
		for _, id := range ids {
			if value.hasAnnotation(id) {
				return true
			}
		}
		if containsAnnotated(value.children, ids...) {
			return true
		}
	}
	return false
}

// DecryptBundle packages a Kindle download directory as a standalone,
// unencrypted KFX ZIP. The destination must not already exist.
func DecryptBundle(bundle, destination string, key []byte) (stats Stats, err error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return stats, err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return stats, err
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(destination)
		}
	}()
	stats, err = DecryptBundleTo(bundle, file, key)
	if err != nil {
		return stats, err
	}
	if err := file.Sync(); err != nil {
		return stats, err
	}
	if err := file.Close(); err != nil {
		return stats, err
	}
	complete = true
	return stats, nil
}

// DecryptBundleTo writes a standalone, unencrypted KFX ZIP to destination.
// Only DRMION and already-plain CONT publication containers are retained.
// Reader manifests, action state, and DRM vouchers are source-side artifacts;
// excluding them keeps license watermarks and account-bound material out of
// both ordinary intermediates and decrypted debug archives. Callers that need
// those artifacts retain the original encrypted bundle separately.
//
// The function enables callers to keep the intermediate archive in memory or
// stream it through an anonymous pipe without creating a plaintext temporary
// file.
func DecryptBundleTo(bundle string, destination io.Writer, key []byte) (stats Stats, err error) {
	entries, err := os.ReadDir(bundle)
	if err != nil {
		return stats, fmt.Errorf("read bundle: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	writer := zip.NewWriter(destination)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return stats, infoErr
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(bundle, entry.Name()))
		if readErr != nil {
			return stats, readErr
		}
		encrypted := isDRMION(data)
		if encrypted {
			var pages int
			data, pages, readErr = DecryptRecord(data, key)
			if readErr != nil {
				return stats, fmt.Errorf("%s: %w", entry.Name(), readErr)
			}
			stats.EncryptedRecords++
			stats.Pages += pages
		}
		if !encrypted && !bytes.HasPrefix(data, []byte("CONT")) {
			continue
		}
		header, headerErr := zip.FileInfoHeader(info)
		if headerErr != nil {
			return stats, headerErr
		}
		header.Name = filepath.ToSlash(entry.Name())
		header.Method = zip.Store
		header.SetMode(info.Mode().Perm())
		output, createErr := writer.CreateHeader(header)
		if createErr != nil {
			return stats, createErr
		}
		if _, writeErr := output.Write(data); writeErr != nil {
			return stats, writeErr
		}
		stats.Files++
	}
	if stats.EncryptedRecords == 0 {
		return stats, errors.New("bundle contains no DRMION records")
	}
	if err := writer.Close(); err != nil {
		return stats, err
	}
	return stats, nil
}

func isDRMION(data []byte) bool {
	return len(data) >= len(drmIonPrefix)+8 && bytes.Equal(data[:len(drmIonPrefix)], drmIonPrefix)
}
