package debugcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/pflag"

	"leafport/internal/exporter"
	"leafport/internal/library"
)

func runDebugCapture(ctx context.Context, arguments []string, stdout, stderr io.Writer) (err error) {
	flags := pflag.NewFlagSet("debug capture", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	id := flags.String("id", "", "exact downloaded book ID")
	output := flags.String("out", "", "destination .kfx-zip archive")
	appPath := flags.String("app", "/Applications/Amazon Kindle.app", "Amazon Kindle.app path")
	libraryPath := flags.String("library", "", "limit discovery to this eBooks directory")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug capture --id BOOK_ID --out ARCHIVE")
		flags.PrintDefaults()
	}
	if parseErr := flags.Parse(arguments); parseErr != nil {
		if errors.Is(parseErr, pflag.ErrHelp) {
			return nil
		}
		return parseErr
	}
	if flags.NArg() != 0 || *id == "" || *output == "" {
		return errors.New("debug capture requires --id BOOK_ID and --out ARCHIVE")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("debug capture requires macOS")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	roots, err := library.DiscoverRoots(home, *libraryPath)
	if err != nil {
		return err
	}
	books, warnings, err := library.DiscoverBooks(ctx, roots)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "Warning: %v\n", warning)
	}
	var selected *library.Book
	for index := range books {
		if books[index].ID == *id {
			selected = &books[index]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("no downloaded book has ID %q", *id)
	}
	destination, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = errors.New("not a directory")
		}
		return fmt.Errorf("debug capture output directory: %w", statErr)
	}
	workRoot, err := os.MkdirTemp(parent, ".leafport-debug-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(workRoot)) }()
	archive, err := exporter.Export(ctx, exporter.Config{
		AppPath: *appPath, AccountSecret: strings.TrimSpace(os.Getenv("LEAFPORT_ACCOUNT_SECRET")),
		Stdin: os.Stdin, Stdout: stdout, Stderr: stderr,
	}, library.Output{Book: *selected}, workRoot)
	if err != nil {
		return err
	}
	if err := writeCapturedArchive(destination, archive); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Captured debug archive: %s (%d bytes)\n", destination, archive.Size)
	return nil
}

func writeCapturedArchive(destination string, archive exporter.Archive) (err error) {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		err = errors.Join(err, output.Close())
		if !complete {
			err = errors.Join(err, os.Remove(destination))
		}
	}()
	if archive.Path != "" {
		input, openErr := os.Open(archive.Path)
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(output, input)
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
	} else if _, err := output.Write(archive.Data); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	complete = true
	return nil
}
