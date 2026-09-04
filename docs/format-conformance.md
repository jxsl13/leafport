# Format conformance and Kindle compatibility

Leafport separates normative file conformance from reader compatibility. No
offline validator can certify a particular Kindle firmware, so a final
Colorsoft smoke test remains distinct from the reproducible gates below.

## EPUB

Leafport targets the [W3C EPUB 3.3 Recommendation](https://www.w3.org/TR/epub-33/)
and [EPUB Reading Systems 3.3](https://www.w3.org/TR/epub-rs-33/). Output uses
canonical OCF packaging, a single package document, complete local references,
decoded raster images, navigation, and one declared internal cover image. In
line with Amazon's guidance, normalization does not add a second HTML cover.
Missing or unusable cover metadata falls back to the first non-empty spine page,
preferring that page's first image. Existing cover content is moved, not copied.
The repair pass also removes a PostScript-flavoured OpenType font only when its
family is unused outside its own `@font-face`; used embedded fonts are retained.

Image books can be emitted as pre-paginated EPUB with one XHTML viewport and
JPEG per page. Comics additionally carry Amazon's `book-type=comic`, original
resolution, writing-mode, and orientation metadata described in the
[Kindle fixed-layout comic guidance](https://kdp.amazon.com/en_US/help/topic/G9GSTY4LTRT39D4Z).
Spread hints include both EPUB 3.3's `rendition:page-spread-*` aliases and the
unprefixed `page-spread-*` names documented by Amazon for older Kindle readers.
Cover handling follows Amazon's
[cover guidance](https://kdp.amazon.com/en_US/help/topic/G6GTK3T3NUHKLEFX).

Gate: official EPUBCheck 5.3.0 with `--failonwarnings`, plus Leafport's ZIP,
XML, reference, image-decode, spine, and cover checks. MuPDF supplies an
independent rendering pass. Amazon's Kindle Previewer/compiler is an additional
delivery check for EPUB; Amazon does not list PDF as a Previewer input format,
so the final PDF device-mode check requires a physical Kindle.

```sh
leafport validate --input BOOK.epub
java -jar epubcheck.jar --failonwarnings BOOK.epub
mutool draw -q -F png -r 24 -o /dev/null BOOK.epub
```

## PDF

Leafport emits PDF 1.7 under the ISO 32000-1 model. See the
[PDF specification archive](https://pdfa.org/resource/pdf-specification-archive/)
and [Adobe PDF 1.7 reference](https://opensource.adobe.com/dc-acrobat-sdk-docs/pdfstandards/pdfreference1.7old.pdf).
The Kindle compatibility profile uses classic cross-reference tables and no
object streams, encryption, forms, signatures, JavaScript, embedded files,
page transitions, thumbnails, or unsupported annotations. It preserves page
content, dimensions, outlines, metadata, and safe direct, GoTo, HTTP(S), and
mailto links. Non-positive embedded-font ascent/cap-height values are repaired
from the descriptor's own positive `FontBBox` top without rewriting glyphs,
widths, encodings, or page content.

For page-oriented KFX sources, a declared cover outside the reading order is
inserted once. A cover already later in the reading order is moved to the front,
not duplicated. Without declared cover metadata, the first non-empty page is
the PDF cover fallback.

Gate: strict pdfcpu validation, qpdf structural checking, a Ghostscript
all-page interpretation pass, and a MuPDF all-page render pass. qpdf itself
correctly notes that its check is structural rather than a complete PDF
conformance proof.

```sh
leafport validate --input BOOK.pdf
pdfcpu validate -m strict -o BOOK.pdf
qpdf --check BOOK.pdf
gs -q -dSAFER -dNOPAUSE -dBATCH -sDEVICE=nullpage BOOK.pdf
mutool draw -q -F png -r 36 -o /dev/null BOOK.pdf
```

## CBZ

CBZ has no normative standards-body specification. Leafport therefore defines
a conservative interoperability profile: an unencrypted ZIP containing flat,
lexicographically ordered, fully decoded raster pages, optional ZIP-comment
ComicBookInfo JSON, and canonical `ComicInfo.xml`. Page bytes are preserved.
This is a compatibility profile, not a claim of standards certification.
Declared-cover and first-non-empty fallback ordering is shared with PDF and
fixed-layout EPUB.

```sh
leafport validate --input BOOK.cbz
unzip -t BOOK.cbz
```

All validators above run locally. Publication files do not need to be uploaded
to a third-party service.

The repository includes a non-mutating harness for the complete local gate.
`KINDLEGEN` and `EBOOK_CONVERT` are optional delivery checks; the remaining
variables are required when their corresponding format is present:

```sh
LEAFPORT_BIN=./leafport \
JAVA=/path/to/java \
EPUBCHECK_JAR=/path/to/epubcheck.jar \
PDFCPU=/path/to/pdfcpu \
KINDLEGEN=/path/to/kindlegen \
EBOOK_CONVERT=/path/to/ebook-convert \
scripts/validate-publications.sh output/final/*
```

## Safe repair

`leafport fix --input ORIGINAL --output COPY` supports PDF, EPUB, and CBZ
without KFX source material. It creates the destination exclusively and removes
partial output after failure. The input is never opened for writing. The same
normalizers run automatically during new conversions.
