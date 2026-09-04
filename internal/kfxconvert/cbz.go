package kfxconvert

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var cbzEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// CBZResult summarizes a lossless image-comic reconstruction.
type CBZResult struct {
	Pages     int
	Images    int
	Resources int
}

// CBZValidationResult reports the conservative CBZ compatibility profile
// checked by Leafport. CBZ has no normative standards body specification.
type CBZValidationResult struct {
	Entries   int
	Images    int
	ComicInfo bool
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
	pages, _ = fixedLayoutPagesWithCover(book, pages)
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
		header.Modified = cbzEpoch
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
	comicInfo := cbzComicInfo(metadata, len(pages), book != nil && book.pageProgressionDirection() == "rtl")
	if err = writeZIPEntry(archive, "ComicInfo.xml", comicInfo, zip.Deflate); err != nil {
		return result, fmt.Errorf("write CBZ ComicInfo.xml: %w", err)
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

func cbzComicInfo(metadata Metadata, pages int, rtl bool) string {
	manga := "No"
	if rtl {
		manga = "YesAndRightToLeft"
	}
	writers := strings.Join(metadata.Authors, ", ")
	return `<?xml version="1.0" encoding="utf-8"?>
<ComicInfo xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
<Title>` + escapeXML(metadata.Title) + `</Title><Writer>` + escapeXML(writers) + `</Writer><Publisher>` + escapeXML(metadata.Publisher) + `</Publisher><LanguageISO>` + escapeXML(metadata.Language) + `</LanguageISO><PageCount>` + fmt.Sprint(pages) + `</PageCount><Manga>` + manga + `</Manga><Pages><Page Image="0" Type="FrontCover" /></Pages></ComicInfo>`
}

// ValidateCBZFile fully decodes every image and rejects encrypted, duplicate,
// nested, unordered, or non-publication entries.
func ValidateCBZFile(path string) (result CBZValidationResult, err error) {
	return validateCBZFile(path, true)
}

func validateCBZFile(path string, requireCanonicalOrder bool) (result CBZValidationResult, err error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return result, fmt.Errorf("open CBZ ZIP: %w", err)
	}
	defer func() { err = errors.Join(err, archive.Close()) }()
	if len(archive.File) == 0 {
		return result, errors.New("CBZ contains no entries")
	}
	if len(archive.File) > maximumArchiveEntries {
		return result, fmt.Errorf("CBZ has %d entries; safety limit is %d", len(archive.File), maximumArchiveEntries)
	}
	seen := make(map[string]bool, len(archive.File))
	var imageNames []string
	var expandedBytes uint64
	for _, entry := range archive.File {
		if entry.UncompressedSize64 > maximumExpandedArchiveBytes-expandedBytes {
			return result, errors.New("CBZ expanded size exceeds the 16 GiB safety limit")
		}
		expandedBytes += entry.UncompressedSize64
		result.Entries++
		if entry.Flags&1 != 0 {
			return result, fmt.Errorf("CBZ entry %s is encrypted", entry.Name)
		}
		if filepath.Base(entry.Name) != entry.Name || entry.FileInfo().IsDir() || entry.Name == "" {
			return result, fmt.Errorf("CBZ has an unsafe entry path %q", entry.Name)
		}
		foldedName := strings.ToLower(entry.Name)
		if seen[foldedName] {
			return result, fmt.Errorf("CBZ contains duplicate entry %s", entry.Name)
		}
		seen[foldedName] = true
		if strings.EqualFold(entry.Name, "ComicInfo.xml") {
			if (requireCanonicalOrder && entry.Name != "ComicInfo.xml") || result.ComicInfo {
				return result, errors.New("CBZ must contain at most one canonically named ComicInfo.xml")
			}
			data, readErr := readZIPEntryLimited(entry, 1<<20)
			if readErr != nil {
				return result, readErr
			}
			var document struct {
				XMLName xml.Name
			}
			if unmarshalErr := xml.Unmarshal(data, &document); unmarshalErr != nil || document.XMLName.Local != "ComicInfo" {
				return result, fmt.Errorf("CBZ ComicInfo.xml is invalid: %w", errors.Join(unmarshalErr, errors.New("root element must be ComicInfo")))
			}
			result.ComicInfo = true
			continue
		}
		if entry.UncompressedSize64 > 1<<30 {
			return result, fmt.Errorf("CBZ image %s exceeds the 1 GiB safety limit", entry.Name)
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return result, openErr
		}
		configuration, format, decodeErr := image.DecodeConfig(reader)
		closeErr := reader.Close()
		if decodeErr != nil || closeErr != nil {
			return result, fmt.Errorf("inspect CBZ image %s: %w", entry.Name, errors.Join(decodeErr, closeErr))
		}
		if err := validateRasterDimensions(configuration.Width, configuration.Height, entry.Name); err != nil {
			return result, err
		}
		if !cbzExtensionMatchesFormat(entry.Name, format) {
			return result, fmt.Errorf("CBZ image %s has content format %s", entry.Name, format)
		}
		reader, openErr = entry.Open()
		if openErr != nil {
			return result, openErr
		}
		decoded, _, decodeErr := image.Decode(reader)
		closeErr = reader.Close()
		if decodeErr != nil || closeErr != nil {
			return result, fmt.Errorf("decode CBZ image %s: %w", entry.Name, errors.Join(decodeErr, closeErr))
		}
		if decoded.Bounds().Dx() != configuration.Width || decoded.Bounds().Dy() != configuration.Height {
			return result, fmt.Errorf("CBZ image %s decoded with inconsistent dimensions", entry.Name)
		}
		imageNames = append(imageNames, entry.Name)
		result.Images++
	}
	if result.Images == 0 {
		return result, errors.New("CBZ contains no decodable page images")
	}
	if requireCanonicalOrder && !slices.IsSorted(imageNames) {
		return result, errors.New("CBZ page entries are not lexicographically ordered")
	}
	return result, nil
}

func cbzExtensionMatchesFormat(name, format string) bool {
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if format == "jpeg" {
		return extension == "jpg" || extension == "jpeg"
	}
	return extension == strings.ToLower(format)
}

// NormalizeCBZ writes a canonical, non-overwriting CBZ copy with ordered page
// names and ComicInfo.xml. Image bytes are preserved exactly.
func NormalizeCBZ(source, destination string) (result CBZValidationResult, err error) {
	validated, err := validateCBZFile(source, false)
	if err != nil {
		return result, err
	}
	archive, err := zip.OpenReader(source)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, archive.Close()) }()
	output, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if output != nil {
			err = errors.Join(err, output.Close())
		}
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()
	writer := zip.NewWriter(output)
	if archive.Comment != "" {
		if err = writer.SetComment(archive.Comment); err != nil {
			return result, err
		}
	}
	width := max(4, len(fmt.Sprint(validated.Images)))
	page := 0
	var comicInfo []byte
	entries := append([]*zip.File(nil), archive.File...)
	slices.SortFunc(entries, func(left, right *zip.File) int {
		return strings.Compare(left.Name, right.Name)
	})
	for _, entry := range entries {
		if strings.EqualFold(entry.Name, "ComicInfo.xml") {
			comicInfo, err = readZIPEntryLimited(entry, 1<<20)
			if err != nil {
				return result, err
			}
			continue
		}
		data, readErr := readZIPEntryLimited(entry, 1<<30)
		if readErr != nil {
			return result, readErr
		}
		extension, _, ok := cbzImageType(data)
		if !ok {
			return result, fmt.Errorf("CBZ entry %s is not a supported page image", entry.Name)
		}
		page++
		if err = writeZIPBytes(writer, fmt.Sprintf("%0*d.%s", width, page, extension), data, zip.Store); err != nil {
			return result, err
		}
	}
	if len(comicInfo) == 0 {
		comicInfo = []byte(cbzComicInfo(Metadata{Title: strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))}, page, false))
	}
	if err = writeZIPBytes(writer, "ComicInfo.xml", comicInfo, zip.Deflate); err != nil {
		return result, err
	}
	if err = writer.Close(); err != nil {
		return result, err
	}
	if err = output.Sync(); err != nil {
		return result, err
	}
	if err = output.Close(); err != nil {
		output = nil
		return result, err
	}
	output = nil
	result, err = ValidateCBZFile(destination)
	if err != nil {
		return result, err
	}
	complete = true
	return result, nil
}

func readZIPEntryLimited(entry *zip.File, limit uint64) ([]byte, error) {
	if entry.UncompressedSize64 > limit {
		return nil, fmt.Errorf("ZIP entry %s exceeds the safety limit", entry.Name)
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if uint64(len(data)) != entry.UncompressedSize64 {
		return nil, fmt.Errorf("ZIP entry %s has an inconsistent size", entry.Name)
	}
	return data, nil
}

func validateCBZ(path string, pages []Page) error {
	validated, err := ValidateCBZFile(path)
	if err != nil {
		return err
	}
	if validated.Images != len(pages) {
		return fmt.Errorf("reconstructed CBZ has %d pages; expected %d", validated.Images, len(pages))
	}
	if !validated.ComicInfo {
		return errors.New("reconstructed CBZ has no ComicInfo.xml")
	}
	return nil
}
