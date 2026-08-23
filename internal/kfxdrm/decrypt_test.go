package kfxdrm

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ulikunitz/xz/lzma"
)

func TestDecryptRecord(t *testing.T) {
	key := []byte("0123456789abcdef")
	for _, test := range []struct {
		name       string
		plaintext  []byte
		compressed bool
	}{
		{name: "plain", plaintext: []byte("CONT\x01standalone KFX page")},
		{name: "lzma", plaintext: bytes.Repeat([]byte("compressed KFX page\n"), 200), compressed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := makeTestRecord(t, key, test.plaintext, test.compressed)
			got, pages, err := DecryptRecord(record, key)
			if err != nil {
				t.Fatal(err)
			}
			if pages != 1 {
				t.Fatalf("pages = %d, want 1", pages)
			}
			if !bytes.Equal(got, test.plaintext) {
				t.Fatalf("plaintext mismatch: got %d bytes, want %d", len(got), len(test.plaintext))
			}
		})
	}
}

func TestDecryptRecordRejectsWrongKey(t *testing.T) {
	record := makeTestRecord(t, []byte("0123456789abcdef"), []byte("page"), false)
	if _, _, err := DecryptRecord(record, []byte("fedcba9876543210")); err == nil {
		t.Fatal("wrong key was accepted")
	}
}

func TestDecryptBundle(t *testing.T) {
	bundle := t.TempDir()
	key := []byte("0123456789abcdef")
	want := []byte("CONT\x01bundle page")
	if err := os.WriteFile(filepath.Join(bundle, "main.azw8"), makeTestRecord(t, key, want, true), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "metadata.kfx"), []byte("CONT\x01metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"BookManifest.kfx": []byte("SQLite format 3\x00reader manifest"),
		"book.voucher":     []byte("ACCOUNT_SECRET\x00watermark=license-identifier"),
		"StartActions.asc": []byte("reader action state"),
	} {
		if err := os.WriteFile(filepath.Join(bundle, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "book.kfx-zip")
	stats, err := DecryptBundle(bundle, destination, key)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 || stats.EncryptedRecords != 1 || stats.Pages != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	archive, err := zip.OpenReader(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if len(archive.File) != 2 {
		t.Fatalf("archive contains %d entries, want 2 publication containers", len(archive.File))
	}
	for _, file := range archive.File {
		if file.Name == "BookManifest.kfx" || file.Name == "book.voucher" || file.Name == "StartActions.asc" {
			t.Fatalf("source-side artifact was retained: %s", file.Name)
		}
		if file.Name != "main.azw8" {
			continue
		}
		reader, openErr := file.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		var got bytes.Buffer
		if _, copyErr := got.ReadFrom(reader); copyErr != nil {
			t.Fatal(copyErr)
		}
		_ = reader.Close()
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("archive payload = %q, want %q", got.Bytes(), want)
		}
		return
	}
	t.Fatal("main.azw8 not found in output")
}

func TestDecryptBundleToMemory(t *testing.T) {
	bundle := t.TempDir()
	key := []byte("0123456789abcdef")
	want := []byte("CONT\x01in-memory page")
	if err := os.WriteFile(filepath.Join(bundle, "main.azw8"), makeTestRecord(t, key, want, false), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	stats, err := DecryptBundleTo(bundle, &output, key)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.EncryptedRecords != 1 || output.Len() == 0 {
		t.Fatalf("unexpected output: stats=%+v bytes=%d", stats, output.Len())
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestValidateBundleKeyBeforeOutput(t *testing.T) {
	bundle := t.TempDir()
	key := []byte("0123456789abcdef")
	for index := range 4 {
		name := filepath.Join(bundle, fmt.Sprintf("record-%d.azw8", index))
		if err := os.WriteFile(name, makeTestRecord(t, key, []byte("page"), false), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	validation, err := ValidateBundleKey(bundle, key)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Records != 3 || validation.Pages != 3 {
		t.Fatalf("unexpected validation: %+v", validation)
	}
	if _, err := ValidateBundleKey(bundle, []byte("fedcba9876543210")); err == nil {
		t.Fatal("wrong key passed sample validation")
	}
}

func FuzzDecryptRecordDoesNotPanic(f *testing.F) {
	f.Add([]byte("not a DRMION record"))
	f.Add(append(append([]byte(nil), drmIonPrefix...), make([]byte, 8)...))
	key := []byte("0123456789abcdef")
	f.Fuzz(func(t *testing.T, record []byte) {
		_, _, _ = DecryptRecord(record, key)
	})
}

func makeTestRecord(t *testing.T, key, plaintext []byte, compressed bool) []byte {
	t.Helper()
	payload := append([]byte(nil), plaintext...)
	if compressed {
		var compressedData bytes.Buffer
		writer, err := lzma.NewWriter(&compressedData)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		payload = append([]byte{0}, compressedData.Bytes()...)
	}
	payload = padPKCS7(payload, aes.BlockSize)
	iv := []byte("abcdefghijklmnop")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := make([]byte, len(payload))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, payload)

	cipherValue := ionBlob(ciphertext)
	if compressed {
		cipherValue = ionAnnotated(sidCompressedV1, cipherValue)
	}
	page := ionAnnotated(sidEncryptedPageV2, ionContainer(ionStruct,
		append(ionVarUInt(sidCipherText), cipherValue...),
		append(ionVarUInt(sidCipherIV), ionBlob(iv)...),
	))
	envelope := ionAnnotated(sidEnvelopeV2, ionContainer(ionList, page))
	record := append([]byte(nil), drmIonPrefix...)
	record = append(record, ionVersionMarker...)
	record = append(record, envelope...)
	record = append(record, make([]byte, 8)...)
	return record
}

func padPKCS7(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func ionBlob(data []byte) []byte {
	return ionTyped(ionBLOB, data)
}

func ionContainer(typeID byte, parts ...[]byte) []byte {
	return ionTyped(typeID, bytes.Join(parts, nil))
}

func ionAnnotated(symbolID uint64, value []byte) []byte {
	annotations := ionVarUInt(symbolID)
	payload := append(ionVarUInt(uint64(len(annotations))), annotations...)
	payload = append(payload, value...)
	return ionTyped(ionAnnotation, payload)
}

func ionTyped(typeID byte, payload []byte) []byte {
	if len(payload) <= 13 {
		return append([]byte{typeID<<4 | byte(len(payload))}, payload...)
	}
	result := []byte{typeID<<4 | 0x0e}
	result = append(result, ionVarUInt(uint64(len(payload)))...)
	return append(result, payload...)
}

func ionVarUInt(value uint64) []byte {
	var encoded [10]byte
	position := len(encoded)
	position--
	encoded[position] = byte(value&0x7f) | 0x80
	for value >>= 7; value != 0; value >>= 7 {
		position--
		encoded[position] = byte(value & 0x7f)
	}
	return append([]byte(nil), encoded[position:]...)
}
