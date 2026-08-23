package kfxconvert

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

var cbzEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// CBZResult summarizes a lossless image-comic reconstruction.
type CBZResult struct {
	Pages     int
	Images    int
	Resources int
}

// ConvertToCBZ reconstructs an image-backed fixed-layout KFX publication as a
// lossless page archive. It is exposed for maintenance diagnostics; normal
// conversion selects it only when the publication carries an explicit comic
// marker.
func ConvertToCBZ(archivePath, destination string) (CBZResult, error) {
	book, err := loadBook(archivePath)
	if err != nil {
		return CBZResult{}, err
	}
	pages, err := book.fixedLayoutPages()
	if err != nil {
		return CBZResult{}, err
	}
	return convertImageLayoutPagesToCBZ(book, pages, destination, book.publicationMetadata(Metadata{}))
}

type comicBookInfo struct {
	Title     string        `json:"title,omitempty"`
	Publisher string        `json:"publisher,omitempty"`
	Language  string        `json:"language,omitempty"`
	Lang      string        `json:"lang,omitempty"`
	Credits   []comicCredit `json:"credits,omitempty"`
}

type comicCredit struct {
	Person string `json:"person"`
	Role   string `json:"role"`
}

func convertImageLayoutPagesToCBZ(book *decodedBook, pages []Page, destination string, metadata Metadata) (result CBZResult, err error) {
	if len(pages) == 0 {
		return result, errors.New("KFX publication has no image pages")
	}
	file, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()

	archive := zip.NewWriter(file)
	if comment, commentErr := cbzMetadataComment(metadata); commentErr != nil {
		return result, commentErr
	} else if comment != "" {
		if err = archive.SetComment(comment); err != nil {
			return result, fmt.Errorf("set CBZ metadata: %w", err)
		}
	}
	width := max(4, len(fmt.Sprintf("%d", len(pages))))
	locations := make(map[string]bool, len(pages))
	for index, page := range pages {
		extension, mediaType, ok := cbzImageType(page.Data)
		if !ok {
			return result, fmt.Errorf("page %d (resource $%d, format $%d, %q) is not a CBZ-compatible image",
				index+1, page.ResourceID, page.Format, page.Location)
		}
		name := fmt.Sprintf("%0*d.%s", width, index+1, extension)
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o600)
		header.SetModTime(cbzEpoch)
		header.Comment = mediaType
		entry, createErr := archive.CreateHeader(header)
		if createErr != nil {
			return result, fmt.Errorf("create CBZ page %d: %w", index+1, createErr)
		}
		if _, writeErr := entry.Write(page.Data); writeErr != nil {
			return result, fmt.Errorf("write CBZ page %d: %w", index+1, writeErr)
		}
		locations[page.Location] = true
	}
	if err = archive.Close(); err != nil {
		return result, err
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		file = nil
		return result, err
	}
	file = nil
	if err = validateCBZ(destination, pages); err != nil {
		return result, err
	}
	if err = os.Chmod(destination, 0o600); err != nil {
		return result, fmt.Errorf("protect reconstructed CBZ: %w", err)
	}
	result = CBZResult{Pages: len(pages), Images: len(pages), Resources: len(locations)}
	complete = true
	return result, nil
}

func cbzMetadataComment(metadata Metadata) (string, error) {
	info := comicBookInfo{
		Title: metadata.Title, Publisher: metadata.Publisher,
		Language: metadata.Language, Lang: metadata.Language,
	}
	for _, author := range metadata.Authors {
		if author != "" {
			info.Credits = append(info.Credits, comicCredit{Person: author, Role: "Writer"})
		}
	}
	if info.Title == "" && info.Publisher == "" && info.Language == "" && len(info.Credits) == 0 {
		return "", nil
	}
	data, err := json.Marshal(map[string]comicBookInfo{"ComicBookInfo/1.0": info})
	if err != nil {
		return "", fmt.Errorf("encode CBZ metadata: %w", err)
	}
	if len(data) > 65535 {
		return "", errors.New("CBZ metadata exceeds the ZIP comment limit")
	}
	return string(data), nil
}

func cbzImageType(data []byte) (extension, mediaType string, ok bool) {
	extension, mediaType, ok = imageMediaType(data)
	if !ok || extension == "svg" {
		return "", "", false
	}
	return extension, mediaType, true
}

func validateCBZ(path string, pages []Page) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("validate reconstructed CBZ: %w", err)
	}
	defer archive.Close()
	if len(archive.File) != len(pages) {
		return fmt.Errorf("reconstructed CBZ has %d pages; expected %d", len(archive.File), len(pages))
	}
	for index, file := range archive.File {
		if filepath.Base(file.Name) != file.Name || file.FileInfo().IsDir() {
			return fmt.Errorf("reconstructed CBZ page %d has an invalid path %q", index+1, file.Name)
		}
		reader, openErr := file.Open()
		if openErr != nil {
			return fmt.Errorf("validate reconstructed CBZ page %d: %w", index+1, openErr)
		}
		_, copyErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("validate reconstructed CBZ page %d: %w", index+1, errors.Join(copyErr, closeErr))
		}
		if file.UncompressedSize64 != uint64(len(pages[index].Data)) {
			return fmt.Errorf("reconstructed CBZ page %d has %d bytes; expected %d",
				index+1, file.UncompressedSize64, len(pages[index].Data))
		}
	}
	return nil
}
