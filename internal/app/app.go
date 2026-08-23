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

	"leafport/internal/exporter"
	"leafport/internal/kfxconvert"
	"leafport/internal/library"
	"leafport/internal/readerconfig"
)

// Config contains values parsed from the command line.
type Config struct {
	AppPath        string
	Library        string
	Match          string
	Output         string
	Target         string
	AccountSecret  string
	RedactPatterns []string
	List           bool
	Debug          bool
	RedactPersonal bool
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

func tableCell(value string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(value)
}

func runBatch(ctx context.Context, config Config, streams Streams, target string, books []library.Book) (err error) {
	privacy := kfxconvert.PrivacyOptions{
		DetectPersonal: config.RedactPersonal,
		Patterns:       config.RedactPatterns,
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
	defer func() {
		if workRoot != "" {
			if cleanupErr := os.RemoveAll(workRoot); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	var failures []error
	accountSecrets := preflightAccountSecrets(ctx, config, outputs)
	for _, notice := range accountSecrets.notices {
		fmt.Fprintln(streams.Stdout, notice)
	}
	if len(accountSecrets.notices) != 0 {
		fmt.Fprintln(streams.Stdout)
	}
	reporter := batchReporter{output: streams.Stdout}
	for index, output := range outputs {
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
		existing, statErr := existingPublication(output.Path)
		if statErr != nil {
			recordFailure(fmt.Errorf("inspect output: %w", statErr))
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
		archivePath := output.Path + ".kfx-zip"
		var archiveData []byte
		var archiveSize int64
		archiveInfo, archiveErr := os.Stat(archivePath)
		switch {
		case archiveErr == nil && archiveInfo.IsDir():
			recordFailure(fmt.Errorf("intermediate archive is a directory: %s", archivePath))
			continue
		case archiveErr == nil:
			archiveSize = archiveInfo.Size()
			reporter.field("Action", "converting existing decrypted archive")
		case !errors.Is(archiveErr, os.ErrNotExist):
			recordFailure(fmt.Errorf("inspect intermediate archive: %w", archiveErr))
			continue
		default:
			if block := accountSecrets.blocked[output.Book.ID]; block != nil {
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
			exportConfig := exporter.Config{
				AppPath: config.AppPath, AccountSecret: accountSecret,
				Stdin: streams.Stdin, Stdout: io.Discard, Stderr: streams.Stderr,
			}
			archive, exportErr := exporter.Export(ctx, exportConfig, output, workRoot)
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
		options := kfxconvert.ConversionOptions{Privacy: privacy}
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
	blocked       map[string]error
	byPreferences map[string]string
	notices       []string
}

func preflightAccountSecrets(ctx context.Context, config Config, outputs []library.Output) accountSecretPreflight {
	result := accountSecretPreflight{
		blocked:       make(map[string]error),
		byPreferences: make(map[string]string),
	}
	if config.AccountSecret != "" {
		return result
	}
	groups := make(map[string][]library.Book)
	for _, output := range outputs {
		book := output.Book
		if existing, _ := existingPublication(output.Path); existing != "" && !config.Debug {
			continue
		}
		if info, err := os.Stat(output.Path + ".kfx-zip"); err == nil && info.Mode().IsRegular() {
			continue
		}
		requirements, err := readerconfig.InspectVouchers(book.Path)
		if err == nil && requirements.AccountSecret {
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
			result.blocked[book.ID] = errors.New(detail)
		}
		result.notices = append(result.notices, fmt.Sprintf(
			"Credential check: %d selected book(s) require an unavailable account secret.", len(group)))
	}
	return result
}

func existingPublication(base string) (string, error) {
	for _, extension := range []string{".pdf", ".epub", ".cbz"} {
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
