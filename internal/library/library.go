// Package library discovers local book bundles and plans stable output names.
package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jxsl13/leafport/internal/bookmeta"
)

// Book describes one complete locally downloaded book bundle.
type Book struct {
	ID          string
	Title       string
	Path        string
	Preferences string
	Modified    time.Time
}

// Root describes one local reader-library root and its associated metadata.
type Root struct {
	Path        string
	Preferences string
	MetadataDB  string
}

// Output pairs a selected book with its collision-free destination base path.
// The conversion layer adds the selected final-format extension.
type Output struct {
	Book Book
	Path string
}

// DiscoverRoots finds supported reader-library locations for the current user.
func DiscoverRoots(home, override string) ([]Root, error) {
	defaultPreferences := filepath.Join(home,
		"Library/Containers/com.amazon.Lassen/Data/Library/Preferences/com.amazon.Lassen.plist")
	if override != "" {
		root, err := filepath.Abs(override)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(root); err != nil {
			return nil, fmt.Errorf("reader library %s: %w", root, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("reader library is not a directory: %s", root)
		}
		return []Root{{
			Path: root, Preferences: preferencesForRoot(home, root, defaultPreferences),
			MetadataDB: metadataDatabaseForRoot(root),
		}}, nil
	}

	patterns := []string{
		filepath.Join(home, "Library/Containers/*/Data/Library/eBooks"),
		filepath.Join(home, "Library/Group Containers/*/Library/eBooks"),
	}
	candidates := []string{
		filepath.Join(home, "Library/Application Support/Kindle/My Kindle Content"),
		filepath.Join(home, "Documents/My Kindle Content"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, matches...)
	}
	seen := make(map[string]bool, len(candidates))
	var roots []Root
	for _, candidate := range candidates {
		absolute, err := filepath.Abs(candidate)
		if err != nil || seen[absolute] || !isReaderLibraryPath(home, absolute) {
			continue
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.IsDir() {
			continue
		}
		seen[absolute] = true
		roots = append(roots, Root{
			Path:        absolute,
			Preferences: preferencesForRoot(home, absolute, defaultPreferences),
			MetadataDB:  metadataDatabaseForRoot(absolute),
		})
	}
	slices.SortFunc(roots, func(left, right Root) int {
		return cmp.Compare(left.Path, right.Path)
	})
	if len(roots) == 0 {
		return nil, errors.New("no local reader library directory was found")
	}
	return roots, nil
}

// DiscoverBooks finds complete bundles. Metadata lookup failures are returned
// as warnings because a missing title must not prevent decryption by ID.
func DiscoverBooks(ctx context.Context, roots []Root) ([]Book, []error, error) {
	booksByPath := make(map[string]Book)
	var warnings []error
	type storeResult struct {
		store *bookmeta.Store
		err   error
	}
	metadataStores := make(map[string]storeResult)
	closeMetadataStores := func() error {
		var closeErr error
		for path, result := range metadataStores {
			if result.store != nil {
				closeErr = errors.Join(closeErr,
					wrapPathError("close book metadata", path, result.store.Close()))
			}
		}
		return closeErr
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, warnings, errors.Join(err, closeMetadataStores())
		}
		books, rootWarnings, err := discoverBooksInRoot(ctx, root)
		if err != nil {
			return nil, warnings, errors.Join(err, closeMetadataStores())
		}
		warnings = append(warnings, rootWarnings...)
		if len(books) != 0 && root.MetadataDB != "" {
			result, found := metadataStores[root.MetadataDB]
			if !found {
				result.store, result.err = bookmeta.Open(ctx, root.MetadataDB)
				metadataStores[root.MetadataDB] = result
				if result.err != nil {
					warnings = append(warnings, result.err)
				}
			}
			if result.store != nil {
				for index := range books {
					books[index].Title, err = result.store.Title(ctx, books[index].ID)
					if err != nil {
						if ctxErr := ctx.Err(); ctxErr != nil {
							return nil, warnings, errors.Join(ctxErr, closeMetadataStores())
						}
						warnings = append(warnings, fmt.Errorf("read title for %s: %w", books[index].ID, err))
					}
				}
			}
		}
		for _, book := range books {
			booksByPath[book.Path] = book
		}
	}
	if err := closeMetadataStores(); err != nil {
		warnings = append(warnings, err)
	}
	books := make([]Book, 0, len(booksByPath))
	for _, book := range booksByPath {
		books = append(books, book)
	}
	sort.Slice(books, func(i, j int) bool {
		if books[i].Modified.Equal(books[j].Modified) {
			return books[i].Path < books[j].Path
		}
		return books[i].Modified.After(books[j].Modified)
	})
	return books, warnings, nil
}

func wrapPathError(operation, path string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", operation, path, err)
}

// Filter returns books whose title or ID matches pattern.
func Filter(books []Book, pattern string) ([]Book, error) {
	if len(books) == 0 {
		return nil, errors.New("no complete downloaded KFX book was found")
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid --match regular expression: %w", err)
	}
	matched := make([]Book, 0, len(books))
	for _, book := range books {
		if expression.MatchString(book.Title) || expression.MatchString(book.ID) {
			matched = append(matched, book)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("--match %q matched no downloaded books", pattern)
	}
	return matched, nil
}

// PlanOutputs creates collision-free destination base paths under target.
func PlanOutputs(books []Book, target string) []Output {
	bases := make([]string, len(books))
	counts := make(map[string]int)
	for index, book := range books {
		bases[index] = SafeFilename(DisplayTitle(book))
		counts[strings.ToLower(bases[index])]++
	}
	outputs := make([]Output, 0, len(books))
	for index, book := range books {
		base := bases[index]
		if counts[strings.ToLower(base)] > 1 {
			base += " [" + SafeName(book.ID) + "]"
		}
		outputs = append(outputs, Output{Book: book, Path: filepath.Join(target, base)})
	}
	return outputs
}

// DisplayTitle returns a title suitable for user-facing output.
func DisplayTitle(book Book) string {
	if book.Title != "" {
		return book.Title
	}
	return book.ID
}

// SafeName restricts an identifier to portable ASCII filename characters.
func SafeName(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			result.WriteRune(character)
		}
	}
	if result.Len() == 0 {
		return "book"
	}
	return result.String()
}

// SafeFilename preserves readable Unicode while removing path separators and
// limiting the byte length for common macOS filesystems.
func SafeFilename(value string) string {
	var result strings.Builder
	space := false
	separator := false
	for _, character := range strings.TrimSpace(value) {
		switch {
		case character == '/' || character == ':' || character == '\\':
			separator = true
			space = false
		case unicode.IsControl(character), unicode.IsSpace(character):
			space = true
		default:
			if separator && result.Len() > 0 {
				result.WriteString(" - ")
			} else if space && result.Len() > 0 {
				result.WriteByte(' ')
			}
			separator = false
			space = false
			result.WriteRune(character)
		}
	}
	name := strings.Trim(result.String(), " .-")
	if name == "" {
		return "book"
	}
	const maxBytes = 200
	if len(name) > maxBytes {
		name = name[:maxBytes]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
		name = strings.TrimRight(name, " .-")
	}
	return name
}

func isReaderLibraryPath(home, path string) bool {
	for _, parent := range []string{
		filepath.Join(home, "Library/Containers"),
		filepath.Join(home, "Library/Group Containers"),
	} {
		if relative, err := filepath.Rel(parent, path); err == nil && relative != "." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			container := strings.ToLower(strings.SplitN(relative, string(filepath.Separator), 2)[0])
			return strings.Contains(container, "amazon") || strings.Contains(container, "kindle") ||
				strings.Contains(container, "lassen")
		}
	}
	lower := strings.ToLower(path)
	return strings.Contains(lower, string(filepath.Separator)+"kindle"+string(filepath.Separator)) ||
		strings.Contains(lower, "my kindle content")
}

func preferencesForRoot(home, root, fallback string) string {
	containers := filepath.Join(home, "Library/Containers") + string(filepath.Separator)
	if relative, found := strings.CutPrefix(root, containers); found {
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) > 0 && parts[0] != "" {
			candidate := filepath.Join(containers, parts[0], "Data/Library/Preferences", parts[0]+".plist")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return fallback
}

func metadataDatabaseForRoot(root string) string {
	for directory := root; ; directory = filepath.Dir(directory) {
		if filepath.Base(directory) == "Library" {
			candidate := filepath.Join(directory, "Protected", "BookData.sqlite")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
			return ""
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
	}
}

func discoverBooksInRoot(ctx context.Context, root Root) ([]Book, []error, error) {
	absolute, err := filepath.Abs(root.Path)
	if err != nil {
		return nil, nil, err
	}
	type bundleCandidate struct {
		modified   time.Time
		hasContent bool
		hasVoucher bool
	}
	candidates := make(map[string]bundleCandidate)
	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		directory := filepath.Dir(path)
		candidate := candidates[directory]
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".voucher":
			candidate.hasVoucher = true
		case ".azw8":
			info, err := entry.Info()
			if err != nil {
				return err
			}
			candidate.hasContent = true
			if info.ModTime().After(candidate.modified) {
				candidate.modified = info.ModTime()
			}
		default:
			return nil
		}
		candidates[directory] = candidate
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("scan reader library: %w", err)
	}
	books := make([]Book, 0, len(candidates))
	var warnings []error
	for directory, candidate := range candidates {
		if !candidate.hasContent || !candidate.hasVoucher {
			continue
		}
		relative, err := filepath.Rel(absolute, directory)
		if err != nil {
			continue
		}
		id := inferBookID(relative, directory)
		books = append(books, Book{
			ID: id, Path: directory,
			Preferences: root.Preferences, Modified: candidate.modified,
		})
	}
	return books, warnings, nil
}

func inferBookID(relative, directory string) string {
	for part := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if isASIN(part) {
			return strings.ToUpper(part)
		}
	}
	for _, entry := range strings.FieldsFunc(filepath.Base(directory), func(character rune) bool {
		return !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9')
	}) {
		if isASIN(entry) {
			return strings.ToUpper(entry)
		}
	}
	return filepath.Base(filepath.Dir(directory))
}

func isASIN(value string) bool {
	if len(value) != 10 {
		return false
	}
	for _, character := range value {
		if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}
