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
	"time"

	"leafport/internal/exporter"
	"leafport/internal/library"
)

// Config contains values parsed from the command line.
type Config struct {
	AppPath    string
	Library    string
	Match      string
	Target     string
	RenamePDFs string
	List       bool
}

// Streams contains the process streams used by the application and bridge.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

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
		fmt.Fprintln(streams.Stdout, "ID\tTITLE\tMODIFIED\tPATH")
		for _, book := range books {
			fmt.Fprintf(streams.Stdout, "%s\t%s\t%s\t%s\n", book.ID, book.Title,
				book.Modified.Format(time.RFC3339), book.Path)
		}
		return nil
	}
	if config.RenamePDFs != "" {
		count, err := library.RenamePDFs(config.RenamePDFs, books, streams.Stdout)
		if err != nil {
			return err
		}
		fmt.Fprintf(streams.Stdout, "Renamed %d PDF file(s)\n", count)
		return nil
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

func runBatch(ctx context.Context, config Config, streams Streams, target string, books []library.Book) (err error) {
	outputs := library.PlanOutputs(books, target)
	fmt.Fprintf(streams.Stdout, "Selected %d book(s); target: %s\n", len(outputs), target)
	succeeded := 0
	skipped := 0
	workRoot := ""
	defer func() {
		if workRoot != "" {
			err = errors.Join(err, os.RemoveAll(workRoot))
		}
	}()
	var failures []error
	for index, output := range outputs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			failures = append(failures, ctxErr)
			break
		}
		if _, statErr := os.Stat(output.Path); statErr == nil {
			fmt.Fprintf(streams.Stdout, "[%d/%d] Skipped existing: %s\n", index+1, len(outputs), output.Path)
			skipped++
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("inspect output for %s: %w", output.Book.ID, statErr))
			continue
		}
		fmt.Fprintf(streams.Stdout, "[%d/%d] Decrypting %s — %s\n",
			index+1, len(outputs), output.Book.ID, library.DisplayTitle(output.Book))
		if workRoot == "" {
			workRoot, err = os.MkdirTemp(target, ".leafport-work-")
			if err != nil {
				return err
			}
			if err := os.Chmod(workRoot, 0o700); err != nil {
				return err
			}
		}
		exportConfig := exporter.Config{
			AppPath: config.AppPath,
			Stdin:   streams.Stdin, Stdout: streams.Stdout, Stderr: streams.Stderr,
		}
		if exportErr := exporter.Export(ctx, exportConfig, output, workRoot); exportErr != nil {
			failure := fmt.Errorf("%s (%s): %w",
				library.DisplayTitle(output.Book), output.Book.ID, exportErr)
			fmt.Fprintf(streams.Stderr, "Failed: %v\n", failure)
			failures = append(failures, failure)
			continue
		}
		succeeded++
	}
	fmt.Fprintf(streams.Stdout, "Batch complete: %d succeeded, %d skipped, %d failed\n",
		succeeded, skipped, len(failures))
	return errors.Join(failures...)
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
