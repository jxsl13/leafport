package kfxconvert

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

// FixedEPUBResult summarizes a Kindle-compatible image fixed-layout EPUB.
type FixedEPUBResult struct {
	Pages  int
	Images int
}

type fixedEPUBPage struct {
	data         []byte
	width        int
	height       int
	cover        bool
	imagePath    string
	documentPath string
}

// convertImageLayoutPagesToEPUB emits the EPUB 3.3 fixed-layout form used for
// image books and comics. Amazon's fixed-layout profile requires JPEG page
// images, one XHTML document per displayed page, an explicit original
// resolution, and comic metadata for graphic novels.
func convertImageLayoutPagesToEPUB(book *decodedBook, pages []Page, destination string, metadata Metadata, comic bool) (result FixedEPUBResult, err error) {
	if len(pages) == 0 {
		return result, errors.New("KFX publication has no fixed-layout pages")
	}

	ordered, coverAt := fixedLayoutPagesWithCover(book, pages)

	fixedPages := make([]fixedEPUBPage, 0, len(ordered))
	for index, page := range ordered {
		if page.Format == kfxPDFFormat || bytes.HasPrefix(page.Data, []byte("%PDF-")) {
			return result, fmt.Errorf("fixed-layout EPUB page %d is PDF-backed; comic EPUB requires image pages", index+1)
		}
		data, width, height, imageErr := kindleJPEGPage(page.Data)
		if imageErr != nil {
			return result, fmt.Errorf("fixed-layout EPUB page %d (%q): %w", index+1, page.Location, imageErr)
		}
		fixedPages = append(fixedPages, fixedEPUBPage{
			data: data, width: width, height: height, cover: index == coverAt,
			imagePath:    fmt.Sprintf("images/page-%04d.jpg", index+1),
			documentPath: fmt.Sprintf("text/page-%04d.xhtml", index+1),
		})
	}
	if metadata.Identifier == "" {
		metadata.Identifier = "leafport-fixed-layout"
	}
	if metadata.Title == "" {
		metadata.Title = "Untitled"
	}
	if metadata.Language == "" {
		metadata.Language = "und"
	}
	direction := "ltr"
	if book != nil && book.pageProgressionDirection() == "rtl" {
		direction = "rtl"
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
	if err = writeZIPEntry(archive, "mimetype", "application/epub+zip", zip.Store); err != nil {
		return result, err
	}
	container := `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`
	if err = writeZIPEntry(archive, "META-INF/container.xml", container, zip.Deflate); err != nil {
		return result, err
	}
	if err = writeZIPEntry(archive, "OEBPS/styles.css", fixedEPUBCSS, zip.Deflate); err != nil {
		return result, err
	}
	for index, page := range fixedPages {
		kind := "pagebreak"
		if page.cover {
			kind = "cover"
		}
		document := fixedEPUBDocument(metadata.Language, metadata.Title, kind, index+1, page)
		if err = writeZIPEntry(archive, "OEBPS/"+page.documentPath, document, zip.Deflate); err != nil {
			return result, err
		}
		if err = writeZIPBytes(archive, "OEBPS/"+page.imagePath, page.data, zip.Store); err != nil {
			return result, err
		}
	}
	if err = writeZIPEntry(archive, "OEBPS/nav.xhtml", fixedEPUBNavigation(metadata, fixedPages), zip.Deflate); err != nil {
		return result, err
	}
	if err = writeZIPEntry(archive, "OEBPS/content.opf", fixedEPUBPackage(metadata, fixedPages, direction, comic), zip.Deflate); err != nil {
		return result, err
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
	if err = validateEPUB(destination, len(fixedPages), len(fixedPages), 0, 0); err != nil {
		return result, err
	}
	if err = validateFixedEPUBPackage(destination, len(fixedPages), comic); err != nil {
		return result, err
	}
	if _, err = ValidateEPUBFile(destination); err != nil {
		return result, err
	}
	if err = os.Chmod(destination, 0o600); err != nil {
		return result, fmt.Errorf("protect fixed-layout EPUB: %w", err)
	}
	result = FixedEPUBResult{Pages: len(fixedPages), Images: len(fixedPages)}
	complete = true
	return result, nil
}

func kindleJPEGPage(data []byte) ([]byte, int, int, error) {
	configuration, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("inspect page image: %w", err)
	}
	if err := validateRasterDimensions(configuration.Width, configuration.Height, "page image"); err != nil {
		return nil, 0, 0, err
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode page image: %w", err)
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if format == "jpeg" && bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) {
		return data, width, height, nil
	}
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(canvas, canvas.Bounds(), decoded, bounds.Min, draw.Over)
	var output bytes.Buffer
	if err := jpeg.Encode(&output, canvas, &jpeg.Options{Quality: 95}); err != nil {
		return nil, 0, 0, fmt.Errorf("encode Kindle JPEG page: %w", err)
	}
	return output.Bytes(), width, height, nil
}

func fixedEPUBDocument(language, title, kind string, pageNumber int, page fixedEPUBPage) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="` + escapeXML(language) + `" xml:lang="` + escapeXML(language) + `">
<head><meta charset="UTF-8"/><meta name="viewport" content="width=` + strconv.Itoa(page.width) + `,height=` + strconv.Itoa(page.height) + `"/><title>` + escapeXML(title) + ` — ` + strconv.Itoa(pageNumber) + `</title><link rel="stylesheet" type="text/css" href="../styles.css"/></head>
<body><div class="page" role="main" epub:type="` + kind + `"><img src="../` + escapeXML(page.imagePath) + `" alt="Page ` + strconv.Itoa(pageNumber) + `"/></div></body></html>`
}

func fixedEPUBNavigation(metadata Metadata, pages []fixedEPUBPage) string {
	coverHref := pages[0].documentPath
	for _, page := range pages {
		if page.cover {
			coverHref = page.documentPath
			break
		}
	}
	var pageList strings.Builder
	for index, page := range pages {
		pageList.WriteString(`<li><a href="` + escapeXML(page.documentPath) + `">` + strconv.Itoa(index+1) + `</a></li>`)
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="` + escapeXML(metadata.Language) + `" xml:lang="` + escapeXML(metadata.Language) + `">
<head><meta charset="UTF-8"/><title>` + escapeXML(metadata.Title) + `</title></head><body>
<nav epub:type="toc" id="toc"><h1>` + escapeXML(metadata.Title) + `</h1><ol><li><a href="` + escapeXML(coverHref) + `">Cover</a></li></ol></nav>
<nav epub:type="landmarks"><h2>Landmarks</h2><ol><li><a epub:type="cover" href="` + escapeXML(coverHref) + `">Cover</a></li></ol></nav>
<nav epub:type="page-list"><h2>Pages</h2><ol>` + pageList.String() + `</ol></nav>
</body></html>`
}

func fixedEPUBPackage(metadata Metadata, pages []fixedEPUBPage, direction string, comic bool) string {
	var manifest, spine, creators strings.Builder
	manifest.WriteString(`<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/><item id="css" href="styles.css" media-type="text/css"/>`)
	for index, page := range pages {
		imageProperties := ""
		if page.cover {
			imageProperties = ` properties="cover-image"`
		}
		manifest.WriteString(`<item id="page-` + strconv.Itoa(index+1) + `" href="` + page.documentPath + `" media-type="application/xhtml+xml"/>`)
		manifest.WriteString(`<item id="image-` + strconv.Itoa(index+1) + `" href="` + page.imagePath + `" media-type="image/jpeg"` + imageProperties + `/>`)
		spreadProperty := ""
		if index > 0 {
			spread := "page-spread-right rendition:page-spread-right"
			if (index%2 == 0) == (direction == "ltr") {
				spread = "page-spread-left rendition:page-spread-left"
			}
			spreadProperty = ` properties="` + spread + `"`
		}
		spine.WriteString(`<itemref idref="page-` + strconv.Itoa(index+1) + `"` + spreadProperty + `/>`)
	}
	for _, author := range metadata.Authors {
		if strings.TrimSpace(author) != "" {
			creators.WriteString(`<dc:creator>` + escapeXML(author) + `</dc:creator>`)
		}
	}
	publisher := ""
	if metadata.Publisher != "" {
		publisher = `<dc:publisher>` + escapeXML(metadata.Publisher) + `</dc:publisher>`
	}
	comicMeta := ""
	if comic {
		comicMeta = `<meta name="book-type" content="comic"/>`
	}
	writingMode := "horizontal-lr"
	if direction == "rtl" {
		writingMode = "horizontal-rl"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="pub-id">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="pub-id">` + escapeXML(metadata.Identifier) + `</dc:identifier><dc:title>` + escapeXML(metadata.Title) + `</dc:title><dc:language>` + escapeXML(metadata.Language) + `</dc:language>` + creators.String() + publisher + `<meta property="dcterms:modified">` + time.Now().UTC().Format("2006-01-02T15:04:05Z") + `</meta><meta property="rendition:layout">pre-paginated</meta><meta property="rendition:orientation">auto</meta><meta property="rendition:spread">auto</meta><meta name="fixed-layout" content="true"/><meta name="original-resolution" content="` + strconv.Itoa(pages[0].width) + `x` + strconv.Itoa(pages[0].height) + `"/><meta name="orientation-lock" content="none"/><meta name="primary-writing-mode" content="` + writingMode + `"/>` + comicMeta + `</metadata>
<manifest>` + manifest.String() + `</manifest><spine page-progression-direction="` + direction + `">` + spine.String() + `</spine></package>`
}

func validateFixedEPUBPackage(path string, pages int, comic bool) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	packageData, err := readZipFileNamed(archive.File, "OEBPS/content.opf")
	if err != nil {
		return err
	}
	wants := [][]byte{
		[]byte(`<meta property="rendition:layout">pre-paginated</meta>`),
		[]byte(`properties="cover-image"`),
		[]byte(`<meta name="original-resolution"`),
	}
	if comic {
		wants = append(wants, []byte(`<meta name="book-type" content="comic"/>`))
	}
	for _, want := range wants {
		if !bytes.Contains(packageData, want) {
			return fmt.Errorf("validate fixed-layout EPUB: package is missing %s", want)
		}
	}
	if bytes.Count(packageData, []byte("<itemref ")) != pages {
		return fmt.Errorf("validate fixed-layout EPUB: package spine has the wrong page count")
	}
	return nil
}

func readZipFileNamed(files []*zip.File, name string) ([]byte, error) {
	for _, file := range files {
		if file.Name == name {
			return readZipFile(file)
		}
	}
	return nil, fmt.Errorf("missing ZIP entry %s", name)
}

const fixedEPUBCSS = `@page { margin: 0; }
html, body { background: #fff; height: 100%; margin: 0; overflow: hidden; padding: 0; width: 100%; }
.page { height: 100%; margin: 0; padding: 0; width: 100%; }
.page img { display: block; height: 100%; margin: 0; object-fit: contain; padding: 0; width: 100%; }
`
