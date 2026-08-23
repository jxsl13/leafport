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
	fmt.Fprintf(streams.Stdout, "Selected %d book(s); target: %s\n", len(outputs), target)
	debugRoot := ""
	if config.Debug && len(outputs) != 0 {
		debugRoot, err = prepareDebugRoot(target, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(streams.Stdout, "Debug artifacts: %s\n", debugRoot)
		if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
			fmt.Fprintln(streams.Stderr, "Warning: --debug artifacts contain the original encrypted and decrypted data and are not privacy-cleaned.")
		}
	}
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
	accountSecrets := preflightAccountSecrets(ctx, config, streams, outputs)
	for index, output := range outputs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			failures = append(failures, ctxErr)
			break
		}
		existing, statErr := existingPublication(output.Path)
		if statErr != nil {
			failures = append(failures, fmt.Errorf("inspect output for %s: %w", output.Book.ID, statErr))
			continue
		}
		if existing != "" && !config.Debug {
			if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
				failures = append(failures, fmt.Errorf("privacy cleanup was not applied to existing output %s; move it away and run again", existing))
				continue
			}
			fmt.Fprintf(streams.Stdout, "[%d/%d] Skipped existing: %s\n", index+1, len(outputs), existing)
			skipped++
			continue
		}
		debugBookRoot := ""
		if config.Debug {
			debugBookRoot = filepath.Join(debugRoot, library.SafeName(output.Book.ID))
			if debugErr := preserveEncryptedBundle(output.Book.Path, filepath.Join(debugBookRoot, "encrypted")); debugErr != nil {
				failures = append(failures, fmt.Errorf("preserve encrypted debug bundle for %s: %w", output.Book.ID, debugErr))
				continue
			}
			fmt.Fprintf(streams.Stdout, "[%d/%d] Preserved encrypted source: %s\n",
				index+1, len(outputs), filepath.Join(debugBookRoot, "encrypted"))
		}
		archivePath := output.Path + ".kfx-zip"
		var archiveData []byte
		var archiveSize int64
		archiveInfo, archiveErr := os.Stat(archivePath)
		switch {
		case archiveErr == nil && archiveInfo.IsDir():
			failures = append(failures, fmt.Errorf("intermediate archive is a directory: %s", archivePath))
			continue
		case archiveErr == nil:
			archiveSize = archiveInfo.Size()
			fmt.Fprintf(streams.Stdout, "[%d/%d] Converting existing decrypted archive for %s — %s\n",
				index+1, len(outputs), output.Book.ID, library.DisplayTitle(output.Book))
		case !errors.Is(archiveErr, os.ErrNotExist):
			failures = append(failures, fmt.Errorf("inspect intermediate archive for %s: %w", output.Book.ID, archiveErr))
			continue
		default:
			if block := accountSecrets.blocked[output.Book.ID]; block != nil {
				failure := fmt.Errorf("%s (%s): %w",
					library.DisplayTitle(output.Book), output.Book.ID, block)
				failures = append(failures, failure)
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
			fmt.Fprintf(streams.Stdout, "[%d/%d] Decrypting %s — %s\n",
				index+1, len(outputs), output.Book.ID, library.DisplayTitle(output.Book))
			accountSecret := config.AccountSecret
			if accountSecret == "" {
				accountSecret = accountSecrets.byPreferences[output.Book.Preferences]
			}
			exportConfig := exporter.Config{
				AppPath: config.AppPath, AccountSecret: accountSecret,
				Stdin: streams.Stdin, Stdout: streams.Stdout, Stderr: streams.Stderr,
			}
			archive, exportErr := exporter.Export(ctx, exportConfig, output, workRoot)
			if exportErr != nil {
				failure := fmt.Errorf("%s (%s): %w",
					library.DisplayTitle(output.Book), output.Book.ID, exportErr)
				failures = append(failures, failure)
				continue
			}
			archivePath = archive.Path
			archiveData = archive.Data
			archiveSize = archive.Size
			if archive.Path != "" {
				fmt.Fprintf(streams.Stdout, "[%d/%d] Archive exceeded 1 GiB; using private temporary storage\n",
					index+1, len(outputs))
			}
		}
		if config.Debug {
			debugArchive := filepath.Join(debugBookRoot, output.Book.ID+".kfx-zip")
			if debugErr := preserveDecryptedArchive(debugArchive, exporter.Archive{
				Data: archiveData, Path: archivePath, Size: archiveSize,
			}); debugErr != nil {
				failures = append(failures, fmt.Errorf("preserve decrypted debug archive for %s: %w", output.Book.ID, debugErr))
				continue
			}
			fmt.Fprintf(streams.Stdout, "[%d/%d] Preserved decrypted KFX: %s\n",
				index+1, len(outputs), debugArchive)
		}
		if existing != "" {
			if privacy.DetectPersonal || len(privacy.Patterns) != 0 {
				failures = append(failures, fmt.Errorf("privacy cleanup was not applied to existing output %s; move it away and run again", existing))
				continue
			}
			fmt.Fprintf(streams.Stdout, "[%d/%d] Skipped existing final result after debug capture: %s\n",
				index+1, len(outputs), existing)
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
			failure := fmt.Errorf("%s (%s): %w",
				library.DisplayTitle(output.Book), output.Book.ID, convertErr)
			failures = append(failures, failure)
			continue
		}
		switch conversion.Format {
		case "PDF":
			fmt.Fprintf(streams.Stdout, "[%d/%d] Completed PDF: %s (%d pages)\n",
				index+1, len(outputs), conversion.Path, conversion.Pages)
			if conversion.BrokenLinksRemoved != 0 {
				fmt.Fprintf(streams.Stdout, "[%d/%d] Removed %d nonfunctional link annotation(s) inherited from embedded PDF resources\n",
					index+1, len(outputs), conversion.BrokenLinksRemoved)
			}
		case "EPUB":
			fmt.Fprintf(streams.Stdout, "[%d/%d] Completed EPUB: %s (%d sections, %d images, %d media, %d fonts)\n",
				index+1, len(outputs), conversion.Path, conversion.Sections, conversion.Images, conversion.Media, conversion.Fonts)
		case "CBZ":
			fmt.Fprintf(streams.Stdout, "[%d/%d] Completed CBZ: %s (%d pages)\n",
				index+1, len(outputs), conversion.Path, conversion.Pages)
		}
		if conversion.Privacy.Enabled {
			fmt.Fprintf(streams.Stdout,
				"[%d/%d] Privacy cleanup: %d owner value(s) detected, %d private metadata field(s) removed\n",
				index+1, len(outputs), conversion.Privacy.AutomaticValues, conversion.Privacy.MetadataFields)
		}
		succeeded++
	}
	for _, failure := range failures {
		fmt.Fprintf(streams.Stderr, "Failed: %v\n", failure)
	}
	fmt.Fprintf(streams.Stdout, "Batch complete: %d succeeded, %d skipped, %d failed\n",
		succeeded, skipped, len(failures))
	if joined := errors.Join(failures...); joined != nil {
		return reportedError{err: joined}
	}
	return nil
}

type accountSecretPreflight struct {
	blocked       map[string]error
	byPreferences map[string]string
}

func preflightAccountSecrets(ctx context.Context, config Config, streams Streams, outputs []library.Output) accountSecretPreflight {
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
				fmt.Fprintf(streams.Stdout,
					"Account-secret preflight: verified a reader-owned credential against its stored fingerprint (%d file(s), %d candidate(s)).\n",
					discovery.report.Files, discovery.report.Candidates)
				continue
			}
		}
		detail := "voucher requires the raw 40-character account secret; automatic retrieval is unavailable because the signed reader's Data Protection Keychain access group cannot be inherited by Leafport"
		if err != nil {
			detail += "; reader registration: " + err.Error()
		}
		if credentials.HashedAccountSecret != "" {
			detail += "; preferences contain only the incompatible hash"
			if discovery.err != nil {
				detail += "; reader-owned storage scan failed: " + discovery.err.Error()
			} else {
				detail += fmt.Sprintf("; no matching raw value in %d normally readable file(s)", discovery.report.Files)
			}
		}
		detail += "; LEAFPORT_ACCOUNT_SECRET is usable only if the raw value was obtained independently"
		for _, book := range group {
			result.blocked[book.ID] = errors.New(detail)
		}
		fmt.Fprintf(streams.Stderr, "Account-secret preflight: %d selected book(s) require a credential unavailable to this process.\n", len(group))
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
