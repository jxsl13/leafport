# Leafport

Leafport is a single-command, Go-only macOS CLI that discovers locally
downloaded books, decrypts supported titles, and reconstructs a readable PDF,
EPUB, or CBZ named after the book. It needs neither Calibre nor a Windows VM.

The implementation is inspired by this
[Windows tutorial](https://akitaonrails.com/en/2026/07/30/removing-drm-from-kindle-ebooks-in-2026/),
but uses the installed macOS reader runtime through a disposable bridge.
PDF-backed and ordinary image-based fixed-layout publications become PDF;
explicitly marked image comics become lossless CBZ; reflowable publications
become EPUB. EPUBs retain available metadata,
cover, embedded fonts, reading order, RTL direction, navigation, links, tables,
images, ruby, MathML, KVG/SVG, supported KFX styling, and supported audio,
video, button, image-sequence, slideshow, scrollable, and zoomable plugins. PDFs retain title, authors, navigation, RTL
direction, page dimensions, hyperlinks, high-resolution variants, and tiled
images. Embedded PDF links with a missing or invalid local destination are
removed before valid KFX links are rebuilt. CBZs retain original page bytes, reading order, and ComicBookInfo
metadata unless privacy cleanup removes embedded media metadata. A complete
embedded PDF remains byte-identical only when no KFX navigation, links,
privacy cleanup, or other PDF-level enhancements must be applied. Mixed
embedded-PDF and raster pages use a private, automatically removed target-local
work directory. EPUB reconstruction fails closed instead of silently discarding
HTML/webview, unknown interactive plugins, or conditional/magnification layouts.

The format decisions follow [EPUB 3.3](https://www.w3.org/TR/epub-33/) and are
compared with [EPUBCheck](https://www.w3.org/publishing/epubcheck/) and [KFX
Input](https://github.com/kluyg/calibre-kfx-input). Annotated plugin manifests
are decoded with Amazon's [Ion Go](https://github.com/amazon-ion/ion-go).
PDF assembly and validation
use the pure-Go [pdfcpu](https://github.com/pdfcpu/pdfcpu) API. The EPUB writer
is independent because Go EPUB libraries such as
[go-epub](https://github.com/go-shiori/go-epub) and
[publira/epub](https://github.com/publira/epub) create or inspect generic
publications but do not decode KFX. Evaluated Go metadata packages are limited to particular
containers or inspection and do not jointly cover PDF, EPUB, CBZ, rich media,
KFX semantics, and verified removal. Non-Go comparisons include Calibre/KFX Input,
[EbookLib](https://github.com/aerkalov/ebooklib), and
[qpdf](https://github.com/qpdf/qpdf); none is a runtime dependency. The young
[ebook-rs](https://github.com/SV-stark/ebook-rs) advertises broad Rust format
support, but describes its KFX parity only as structural text/assets and does
not provide the macOS reader credential path required here. JPEG-XR is
rejected explicitly because no independently validated pure-Go decoder is
currently suitable.

Current limitation: newer vouchers may require a 40-character raw account
secret. Reader 7.65 stores it as account `kindle.accountsecret.item`, service
`com.amazon.Lassen.KeychainUI`, in its team-bound Data Protection Keychain;
preferences contain only an incompatible 32-character hash. The raw value
passes through `AuthenticationManager.setAccountSecret:` and then into
`KRFDRMDataProvider`; the later OpenSSL decrypt call exposes only a derived
per-book key. Leafport verifies from the Objective-C call graph that the stored
value is `MD5(raw-secret)` and safely tests exact 40-hex candidates found in
normally readable reader-owned files without printing them. Reader 7.65 on the
verified system contains no matching value; its disabled file-keychain fallback
also has no `userDataDict.dat`. A random 160-bit value cannot practically be
recovered from its MD5 fingerprint. Apple documents that Mac Catalyst uses the [Data Protection
Keychain](https://developer.apple.com/documentation/technotes/tn3137-on-mac-keychains),
whose access groups come from restricted code-signing
[entitlements](https://developer.apple.com/documentation/security/sharing-access-to-keychain-items-among-a-collection-of-apps).
The installed release also has `get-task-allow=false`, so SIP prevents an
[external debugger or injector](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.security.cs.debugger)
from attaching. Root alone changes neither rule.
`doctor` verifies this boundary without retaining a credential: after the
ordinary disposable bridge reports no raw value, a second disposable bridge
with the copied reader entitlements is terminated by macOS before returning data.
Without that raw value, Leafport exports DSN-only books and fails closed for
`ACCOUNT_SECRET` books. `doctor` reports the affected count. If the raw value
was obtained independently, `LEAFPORT_ACCOUNT_SECRET` can supply it; Leafport
transfers the value over an anonymous pipe, not in process arguments, and
removes it from the child environment. This is an expert compatibility input,
not an end-user retrieval path, and Leafport never prompts for it.

## Requirements

- Apple silicon Mac
- Go 1.24 or newer
- Kindle for Mac with the books downloaded and locally openable

Verified with macOS 26.5.1 and Kindle 7.65 build 1.465815.10.

## Usage

From the directory containing `go.mod`, export every downloaded book:

```sh
CGO_ENABLED=0 go run . --target ./decrypted-books
```

Or build the single CLI once:

```sh
CGO_ENABLED=0 go build -o leafport .
./leafport --target ./decrypted-books
```

Check discovery, registration, the installed build, runtime interfaces, crypto
symbol ownership, and the prospective hook prologue without opening a book:

```sh
./leafport doctor
```

`--target` is required. Without `--match`, `.*` selects every book. Existing
PDF/EPUB/CBZ results are skipped and never overwritten. The decrypted intermediate
stays in RAM up to 1 GiB; larger archives spill into a private
`.leafport-work-*` directory below the target, which is removed after the
batch.

Retain both the untouched encrypted source bundle and decrypted KFX archive for
one analysis run while still producing the normal PDF/EPUB/CBZ result:

```sh
./leafport --target ./decrypted-books --match B0CJGD6G7L --debug
```

Artifacts are written with private permissions below
`TARGET/debug/TIMESTAMP/BOOK_ID/` as `encrypted/` and `BOOK_ID.kfx-zip`. The
encrypted copy contains the original voucher, manifest, and reader state; the
decrypted KFX archive contains only publication `CONT` containers and therefore
does not duplicate account-bound voucher watermarks. Both still contain
sensitive book data and are never removed automatically. With
`--debug`, an existing final PDF/EPUB/CBZ is not overwritten, but its source and
decrypted archive are captured again.

List discovered books and their IDs:

```sh
./leafport --list
```

The default table contains only IDs and complete titles. Add modification times
and paths with:

```sh
./leafport --list -o wide
```

Select titles or IDs with a Go regular expression:

```sh
./leafport --target ./decrypted-books \
  --match '(?i)künstliche intelligenz|B0CJGD6G7L'
```

Remove likely owner metadata and email addresses, with optional repeatable Go
regular expressions for known names or addresses:

```sh
./leafport --target ./decrypted-books --redact-personal \
  --redact '(?i)Jane Doe' --redact '(?i)jane@example\.com'
```

This redacts reconstructed text, navigation, links, publication metadata, and
matching output titles while preserving KFX text offsets. It also strips PDF
document information, properties, XMP, the source file ID, forms, annotations,
attachments, active actions, and potentially identifying metadata from JPEG,
PNG, GIF, WebP, SVG, MP3, WAV, and MP4 assets. Unsupported embedded Ogg, WebM,
or MPEG metadata fails closed. Automatic detection is conservative: it uses
explicit owner/account/customer/watermark fields and email addresses only when
they occur in such a field or in contextual text such as “licensed to” or
“delivered to”. A bare author or publisher contact address is not classified as
the owner.
Ordinary final exports already omit the account-bound DRM voucher and its opaque
watermark. Privacy cleanup is therefore optional and primarily intended for
explicit user-supplied patterns or unusual publications with visible owner
markers; it is not enabled by default.
Personal data inside embedded PDF page-content streams or image pixels cannot
be reliably detected without content rewriting or OCR and pixel modification;
it is not claimed as removed. Existing outputs are not overwritten; when
privacy cleanup is requested, an existing result is reported as a failure
instead of being silently accepted. `--debug` artifacts intentionally retain
the original sensitive source data.

Use `--library PATH` to restrict discovery to one directory. Final files are
named from local title metadata automatically.

If the raw device secret was obtained independently, supply it in the command
environment:

```sh
LEAFPORT_ACCOUNT_SECRET='40-character-secret' ./leafport --target ./decrypted-books
```

Maintenance commands for future binary analysis remain part of the same CLI:

```sh
./leafport debug analyze --help
./leafport debug disasm --help
./leafport debug dylibify --help
./leafport debug capture --help
./leafport debug credentials --help
./leafport debug kfx --help
./leafport debug pdf --help
./leafport debug objc --help
```

`debug credentials` reports only scan counts and whether the stored fingerprint
has a verified local preimage. `debug objc --calls --references` resolves
selectors, classes, and constant strings for future reader-build audits.

For future converter audits, list only the semantic IDs used by a decrypted
archive, without printing book text:

```sh
./leafport debug kfx --in BOOK.kfx-zip --features
```

`--extract-resources DIRECTORY` retains untouched embedded PDFs, images, fonts,
or media with private permissions for source-vs-output diagnostics.

## Implementation and compatibility

Leafport reads titles through `database/sql` and the cgo-free
`modernc.org/sqlite` driver. It creates a disposable, ad-hoc-signed copy of the
reader runtime, captures the per-book content key in memory, decrypts DRMION
records in Go, parses Binary Ion/KFX in Go, and writes validated PDF, EPUB, or
CBZ output. EPUB packaging follows EPUB 3/OCF rules, including an uncompressed,
descriptor-free first `mimetype` entry. Reconstructed navigation includes the
TOC, typed landmarks, page lists, exact text-offset anchors, noterefs, and
endnotes; table spans are resolved through inherited KFX styles. The installed
app is not modified.

Support remains version-sensitive. Verified builds are matched by both Mach-O
UUID and `__text` hash. Unknown builds use a strict semantic fallback that
independently validates classes, method signatures, symbol identity and owner,
and relocatable arm64 hook instructions; ambiguous or incompatible updates fail
closed. A captured key must also decrypt up to three real encrypted records in
memory before Leafport creates an output. The current implementation supports
the arm64 Mac Catalyst app and DRMION v1/v2.

Use Leafport only for books and purposes permitted in your jurisdiction. Do not
distribute decrypted output.

## Development

The root contains the single entry point; implementation packages are private
under `internal/`.

```sh
gofmt -w .
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build .
go test -race ./...
govulncheck ./...
```

Fuzz targets:

```sh
go test ./internal/kfxdrm -fuzz=FuzzDecryptRecordDoesNotPanic
go test ./internal/library -fuzz=FuzzSafeFilename
```
