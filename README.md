# Leafport

Leafport exports locally downloaded KFX books from the native reader app on
Apple-silicon macOS. It is a Go-only adaptation of the idea in this
[Windows tutorial](https://akitaonrails.com/en/2026/07/30/removing-drm-from-kindle-ebooks-in-2026/):
no Windows VM, Calibre, plugins, cgo, root access, or Keychain access is needed.

The result is an unencrypted `.kfx-zip` named after the book title, with its ID
as fallback. Leafport does not convert KFX to PDF or EPUB.

## Requirements

- Apple-silicon macOS
- Go 1.25 or newer
- Kindle for Mac with the books downloaded and locally openable

Verified with macOS 26.5.1 and Kindle 7.65 build 1.465815.10. The integration
test exported 11 DRMION records and 5,660 pages byte-for-byte identically to the
reference archive.

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

`--target` is required for export. Without `--match`, the default expression
`.*` selects all books. Existing results are skipped and never overwritten.
Temporary files are created with private permissions below the target as
`.leafport-work-*` and removed after the batch.

List discovered books and their IDs:

```sh
./leafport --list
```

Select titles or IDs with a Go regular expression:

```sh
./leafport --target ./decrypted-books \
  --match '(?i)künstliche intelligenz|B0CJGD6G7L'
```

Use `--library PATH` to restrict automatic discovery to one directory. Rename
existing ID-prefixed PDF files from local title metadata with:

```sh
./leafport --rename-pdfs ./output
```

Maintenance commands for future binary analysis remain part of the same CLI:

```sh
./leafport debug disasm --help
./leafport debug dylibify --help
```

## Implementation and compatibility

Leafport reads titles through `database/sql` and the cgo-free
`modernc.org/sqlite` driver. It creates a disposable, ad-hoc-signed copy of the
reader runtime, captures the per-book content key in memory, decrypts DRMION
records in Go, and removes the runtime copy afterward. The installed app is not
modified.

Support is intentionally version-specific: the bridge validates the expected
arm64 crypto entry point and fails closed after incompatible app updates. The
current implementation supports the arm64 Mac Catalyst app and DRMION v1/v2.

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
