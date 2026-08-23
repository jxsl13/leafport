package kfxconvert

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// Inspection describes the publication material present in a decrypted KFX
// archive without altering it.
type Inspection struct {
	Containers     int
	Entities       int
	RawMedia       int
	PDFResources   int
	ImageResources int
	FontResources  int
}

// InspectArchive validates every CONT container and classifies its raw assets.
func InspectArchive(path string) (Inspection, error) {
	var result Inspection
	archive, err := zip.OpenReader(path)
	if err != nil {
		return result, fmt.Errorf("open decrypted KFX archive: %w", err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if !isKFXContainerName(file.Name) {
			continue
		}
		data, readErr := readZipFile(file)
		if readErr != nil {
			return result, readErr
		}
		if !bytes.HasPrefix(data, []byte("CONT")) {
			continue
		}
		container, parseErr := ParseContainer(data)
		if parseErr != nil {
			return result, fmt.Errorf("%s: %w", file.Name, parseErr)
		}
		result.Containers++
		result.Entities += len(container.Entities)
		for _, entity := range container.Entities {
			switch entity.Type {
			case kfxRawMediaType:
				result.RawMedia++
				switch detectMedia(entity.Payload) {
				case "pdf":
					result.PDFResources++
				case "image":
					result.ImageResources++
				}
			case kfxRawFontType:
				result.FontResources++
			}
		}
	}
	if result.Containers == 0 {
		return result, errors.New("archive contains no DRM-free KFX CONT container")
	}
	return result, nil
}

func isKFXContainerName(name string) bool {
	switch filepath.Ext(name) {
	case ".azw8", ".azw9", ".res", ".md", ".kfx":
		return filepath.Base(name) != "BookManifest.kfx"
	default:
		return false
	}
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file.Name, err)
	}
	return data, nil
}

func detectMedia(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}),
		bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")),
		len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image"
	default:
		return "unknown"
	}
}
