// Package app orchestrates Leafport's user-facing operations.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jxsl13/leafport/internal/exporter"
	"github.com/jxsl13/leafport/internal/kfxconvert"
	"github.com/jxsl13/leafport/internal/library"
	"github.com/jxsl13/leafport/internal/readerconfig"
)

// Config contains values parsed from the command line.
type Config struct {
	AppPath         string
	Library         string
	Match           string
	Format          string
	Output          string
	Target          string
	AccountSecret   string
	RedactPatterns  []string
	PreparedPrivacy *kfxconvert.PrivacyOptions
	List            bool
	Debug           bool
	RedactPersonal  bool
}

// Streams contains the process streams used by the application and bridge.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type reportedError struct {
	err error
}

func (err reportedError) Error() string         { return err.err.Error() }
func (err reportedError) Unwrap() error         { return err.err }
func (err reportedError) AlreadyReported() bool { return true }

// Run executes one user-facing Leafport operation.
func Run(ctx context.Context, config Config, streams Streams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("leafport requires macOS")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	roots, err := library.DiscoverRoots(home, config.Library)
	if err != nil {
		return err
	}
	books, warnings, err := library.DiscoverBooks(ctx, roots)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(streams.Stderr, "Warning: %v\n", warning)
	}
	if config.List {
		return writeBookList(streams.Stdout, books, config.Output == "wide")
	}
	if config.Match == "" {
		config.Match = ".*"
	}
	selected, err := library.Filter(books, config.Match)
	if err != nil {
		return err
	}
	if config.Target == "" {
		return errors.New("--target is required for decryption")
	}
	target, err := prepareTargetDirectory(config.Target)
	if err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(config.AppPath, "Contents/MacOS/Kindle"),
		filepath.Join(config.AppPath, "Contents/Resources"),
	} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("required component %s: %w", path, err)
		}
	}
	return runBatch(ctx, config, streams, target, selected)
}

func writeBookList(output io.Writer, books []library.Book, wide bool) error {
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if wide {
		if _, err := fmt.Fprintln(table, "ID\tMODIFIED\tTITLE\tPATH"); err != nil {
			return err
		}
		for _, book := range books {
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", book.ID,
				book.Modified.Format(time.RFC3339), tableCell(library.DisplayTitle(book)), book.Path); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintln(table, "ID\tTITLE"); err != nil {
			return err
		}
		for _, book := range books {
			if _, err := fmt.Fprintf(table, "%s\t%s\n", book.ID,
				tableCell(library.DisplayTitle(book))); err != nil {
				return err
			}
		}
	}
	return table.Flush()
}

var tableCellReplacer = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")

func tableCell(value string) string {
	return tableCellReplacer.Replace(value)
}

func runBatch(ctx context.Context, config Config, streams Streams, target string, books []library.Book) (err error) {
	privacy := kfxconvert.PrivacyOptions{
		DetectPersonal: config.RedactPersonal,
		Patterns:       config.RedactPatterns,
	}
	if config.PreparedPrivacy != nil {
		privacy = *config.PreparedPrivacy
	} else {
		privacy, err = kfxconvert.PreparePrivacyOptions(privacy)
		if err != nil {
			return err
		}
	}
	if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
		books = append([]library.Book(nil), books...)
		for index := range books {
			redacted, redactErr := kfxconvert.RedactText(books[index].Title, privacy)
			if redactErr != nil {
				return redactErr
			}
			books[index].Title = redacted
		}
	}
	outputs := library.PlanOutputs(books, target)
	outputStates := inspectBatchOutputs(outputs, config.Format)
	fmt.Fprintf(streams.Stdout, "Exporting %d book(s)\nTarget: %s\n", len(outputs), target)
	debugRoot := ""
	if config.Debug && len(outputs) != 0 {
		debugRoot, err = prepareDebugRoot(target, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(streams.Stdout, "Debug: %s\n", debugRoot)
		if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
			fmt.Fprintln(streams.Stderr, "Warning: --debug artifacts contain the original encrypted and decrypted data and are not privacy-cleaned.")
		}
	}
	fmt.Fprintln(streams.Stdout)
	succeeded := 0
	skipped := 0
	workRoot := ""
	var exportBatch *exporter.Batch
	var exportBatchErr error
	defer func() {
		if exportBatch != nil {
			if cleanupErr := exportBatch.Close(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
		if workRoot != "" {
			if cleanupErr := os.RemoveAll(workRoot); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	var failures []error
	accountSecrets := preflightAccountSecrets(ctx, config, outputs, outputStates)
	for _, notice := range accountSecrets.notices {
		fmt.Fprintln(streams.Stdout, notice)
	}
	if len(accountSecrets.notices) != 0 {
		fmt.Fprintln(streams.Stdout)
	}
	reporter := batchReporter{output: streams.Stdout}
	for index, output := range outputs {
		state := outputStates[index]
		if ctxErr := ctx.Err(); ctxErr != nil {
			failures = append(failures, ctxErr)
			break
		}
		reporter.begin(index+1, len(outputs), library.DisplayTitle(output.Book), output.Book.ID)
		recordFailure := func(cause error) {
			failures = append(failures, fmt.Errorf("%s (%s): %w",
				library.DisplayTitle(output.Book), output.Book.ID, cause))
			reporter.field("Status", "failed")
			reporter.field("Reason", cause.Error())
		}
		existing := state.existing
		if state.existingErr != nil {
			recordFailure(fmt.Errorf("inspect output: %w", state.existingErr))
			continue
		}
		if existing != "" && !config.Debug {
			if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
				recordFailure(fmt.Errorf("privacy cleanup was not applied to existing output %s; move it away and run again", existing))
				continue
			}
			reporter.field("Status", "skipped (already exists)")
			reporter.field("Output", existing)
			skipped++
			continue
		}
		debugBookRoot := ""
		if config.Debug {
			debugBookRoot = filepath.Join(debugRoot, library.SafeName(output.Book.ID))
			if debugErr := preserveEncryptedBundle(output.Book.Path, filepath.Join(debugBookRoot, "encrypted")); debugErr != nil {
				recordFailure(fmt.Errorf("preserve encrypted debug bundle: %w", debugErr))
				continue
			}
			reporter.field("Encrypted", filepath.Join(debugBookRoot, "encrypted"))
		}
		archivePath := state.archivePath
		var archiveData []byte
		archiveSize := state.archiveSize
		switch {
		case state.archiveErr != nil:
			recordFailure(state.archiveErr)
			continue
		case state.archiveExists:
			reporter.field("Action", "converting existing decrypted archive")
		default:
			if block := accountSecrets.blocked[output.Book.Path]; block != nil {
				recordFailure(block)
				continue
			}
			if workRoot == "" {
				workRoot, err = os.MkdirTemp(target, ".leafport-work-")
				if err != nil {
					return err
				}
				if err := os.Chmod(workRoot, 0o700); err != nil {
					return err
				}
			}
			reporter.field("Action", "decrypting and reconstructing publication")
			accountSecret := config.AccountSecret
			if accountSecret == "" {
				accountSecret = accountSecrets.byPreferences[output.Book.Preferences]
			}
			if exportBatch == nil && exportBatchErr == nil {
				exportBatch, exportBatchErr = exporter.NewBatch(exporter.Config{
					AppPath: config.AppPath,
					Stdin:   streams.Stdin, Stdout: io.Discard, Stderr: streams.Stderr,
				}, workRoot)
			}
			if exportBatchErr != nil {
				recordFailure(exportBatchErr)
				continue
			}
			var accountSecretRequired *bool
			if required, known := accountSecrets.voucherAccountSecret[output.Book.Path]; known {
				accountSecretRequired = &required
			}
			archive, exportErr := exportBatch.ExportWithOptions(ctx, output, exporter.BookOptions{
				AccountSecret: accountSecret, AccountSecretRequired: accountSecretRequired,
			})
			if exportErr != nil {
				recordFailure(exportErr)
				continue
			}
			archivePath = archive.Path
			archiveData = archive.Data
			archiveSize = archive.Size
			if archive.Path != "" {
				reporter.field("Storage", "archive exceeded 1 GiB; using private temporary storage")
			}
		}
		if config.Debug {
			debugArchive := filepath.Join(debugBookRoot, output.Book.ID+".kfx-zip")
			if debugErr := preserveDecryptedArchive(debugArchive, exporter.Archive{
				Data: archiveData, Path: archivePath, Size: archiveSize,
			}); debugErr != nil {
				recordFailure(fmt.Errorf("preserve decrypted debug archive: %w", debugErr))
				continue
			}
			reporter.field("Decrypted", debugArchive)
		}
		if existing != "" {
			if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
				recordFailure(fmt.Errorf("privacy cleanup was not applied to existing output %s; move it away and run again", existing))
				continue
			}
			reporter.field("Status", "skipped (debug artifacts refreshed)")
			reporter.field("Output", existing)
			skipped++
			continue
		}
		metadata := kfxconvert.Metadata{
			Identifier: output.Book.ID, Title: library.DisplayTitle(output.Book),
		}
		var conversion kfxconvert.ConversionResult
		var convertErr error
		options := kfxconvert.ConversionOptions{Privacy: privacy, Format: config.Format}
		if len(archiveData) != 0 {
			conversion, convertErr = kfxconvert.ConvertBytesWithOptions(archiveData, output.Path, metadata, options)
		} else {
			conversion, convertErr = kfxconvert.ConvertWithOptions(archivePath, output.Path, metadata, options)
		}
		if convertErr != nil {
			recordFailure(convertErr)
			continue
		}
		reporter.field("Status", "completed")
		reporter.field("Output", conversion.Path)
		switch conversion.Format {
		case "PDF":
			reporter.field("Format", fmt.Sprintf("PDF · %d pages", conversion.Pages))
			if conversion.BrokenLinksRemoved != 0 {
				reporter.field("Links", fmt.Sprintf("removed %d nonfunctional inherited annotation(s)", conversion.BrokenLinksRemoved))
			}
		case "EPUB":
			reporter.field("Format", fmt.Sprintf("EPUB · %d sections · %d images · %d media · %d fonts",
				conversion.Sections, conversion.Images, conversion.Media, conversion.Fonts))
		case "CBZ":
			reporter.field("Format", fmt.Sprintf("CBZ · %d pages", conversion.Pages))
		case "EPUB-COMIC", "EPUB-FXL":
			reporter.field("Format", fmt.Sprintf("%s · %d pages", conversion.Format, conversion.Pages))
		}
		if conversion.Privacy.Enabled {
			reporter.field("Privacy", fmt.Sprintf("%d owner value(s) detected · %d private metadata field(s) removed",
				conversion.Privacy.AutomaticValues, conversion.Privacy.MetadataFields))
		}
		succeeded++
	}
	reporter.finish()
	fmt.Fprintf(streams.Stdout, "Summary: %d completed · %d skipped · %d failed\n",
		succeeded, skipped, len(failures))
	if joined := errors.Join(failures...); joined != nil {
		return reportedError{err: joined}
	}
	return nil
}

type accountSecretPreflight struct {
	blocked              map[string]error
	byPreferences        map[string]string
	voucherAccountSecret map[string]bool
	notices              []string
}

type batchOutputState struct {
	existing      string
	existingErr   error
	archivePath   string
	archiveSize   int64
	archiveExists bool
	archiveErr    error
}

func inspectBatchOutputs(outputs []library.Output, format string) []batchOutputState {
	states := make([]batchOutputState, len(outputs))
	for index, output := range outputs {
		state := batchOutputState{archivePath: output.Path + ".kfx-zip"}
		state.existing, state.existingErr = existingPublication(output.Path, format)
		info, err := os.Stat(state.archivePath)
		switch {
		case err == nil && info.IsDir():
			state.archiveErr = fmt.Errorf("intermediate archive is a directory: %s", state.archivePath)
		case err == nil:
			state.archiveExists = true
			state.archiveSize = info.Size()
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			state.archiveErr = fmt.Errorf("inspect intermediate archive: %w", err)
		}
		states[index] = state
	}
	return states
}

func preflightAccountSecrets(ctx context.Context, config Config, outputs []library.Output, states []batchOutputState) accountSecretPreflight {
	result := accountSecretPreflight{
		blocked:              make(map[string]error),
		byPreferences:        make(map[string]string),
		voucherAccountSecret: make(map[string]bool),
	}
	if config.AccountSecret != "" {
		return result
	}
	groups := make(map[string][]library.Book)
	for index, output := range outputs {
		book := output.Book
		state := states[index]
		if state.existingErr != nil || state.archiveErr != nil || state.existing != "" && !config.Debug {
			continue
		}
		if state.archiveExists {
			continue
		}
		requirements, err := readerconfig.InspectVouchers(book.Path)
		if err != nil {
			result.blocked[book.Path] = err
			continue
		}
		result.voucherAccountSecret[book.Path] = requirements.AccountSecret
		if requirements.AccountSecret {
			groups[book.Preferences] = append(groups[book.Preferences], book)
		}
	}
	type discoveryResult struct {
		secret string
		report readerconfig.AccountSecretDiscovery
		err    error
	}
	discoveries := make(map[string]discoveryResult)
	home, homeErr := os.UserHomeDir()
	for preferences, group := range groups {
		if err := ctx.Err(); err != nil {
			break
		}
		credentials, err := readerconfig.LoadCredentials(preferences)
		var discovery discoveryResult
		if err == nil && credentials.HashedAccountSecret != "" {
			var found bool
			discovery, found = discoveries[credentials.HashedAccountSecret]
			if !found {
				if homeErr != nil {
					discovery.err = homeErr
				} else {
					discovery.secret, discovery.report, discovery.err = readerconfig.DiscoverAccountSecret(
						credentials.HashedAccountSecret, readerconfig.DefaultAccountSecretRoots(home))
				}
				discoveries[credentials.HashedAccountSecret] = discovery
			}
			if discovery.err == nil && discovery.secret != "" {
				result.byPreferences[preferences] = discovery.secret
				result.notices = append(result.notices, fmt.Sprintf(
					"Credential check: verified a reader-owned account secret (%d files checked).",
					discovery.report.Files))
				continue
			}
		}
		detail := "account secret required; no verified raw credential is available"
		if err != nil {
			detail += "; reader registration unavailable: " + err.Error()
		}
		if credentials.HashedAccountSecret != "" {
			if discovery.err != nil {
				detail += "; local credential scan failed: " + discovery.err.Error()
			} else {
				detail += fmt.Sprintf(" (%d reader files checked)", discovery.report.Files)
			}
		}
		detail += "; run \"leafport doctor\" for details"
		for _, book := range group {
			result.blocked[book.Path] = errors.New(detail)
		}
		result.notices = append(result.notices, fmt.Sprintf(
			"Credential check: %d selected book(s) require an unavailable account secret.", len(group)))
	}
	return result
}

func existingPublication(base, format string) (string, error) {
	extensions := []string{".pdf", ".epub", ".cbz"}
	switch format {
	case "pdf":
		extensions = []string{".pdf"}
	case "epub", "comic-epub":
		extensions = []string{".epub"}
	case "cbz":
		extensions = []string{".cbz"}
	}
	for _, extension := range extensions {
		path := base + extension
		info, err := os.Stat(path)
		switch {
		case err == nil && info.IsDir():
			return "", fmt.Errorf("output is a directory: %s", path)
		case err == nil:
			return path, nil
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return "", err
		}
	}
	return "", nil
}

func prepareTargetDirectory(path string) (string, error) {
	target, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("target is not a directory: %s", target)
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(target, 0o700); err != nil {
			return "", fmt.Errorf("create target directory: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("inspect target directory: %w", err)
	}
	return target, nil
}
