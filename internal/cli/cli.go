// Package cli parses Leafport commands and maps failures to process exit codes.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"leafport/internal/app"
	"leafport/internal/debugcmd"
	"leafport/internal/exporter"
)

// Run executes Leafport with explicit arguments and streams.
func Run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := run(ctx, arguments, stdin, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "leafport: %v\n", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		arguments = []string{"leafport"}
	}
	if len(arguments) > 1 {
		switch arguments[1] {
		case exporter.RuntimeCommand:
			return exporter.RunRuntime(arguments[2:], stdout)
		case "debug":
			return debugcmd.Run(arguments[2:], stdout, stderr)
		}
	}
	config, err := parse(arguments[0], arguments[1:], stderr)
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	return app.Run(ctx, config, app.Streams{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

func parse(program string, arguments []string, output io.Writer) (app.Config, error) {
	config := app.Config{
		AppPath: "/Applications/Amazon Kindle.app",
		Match:   ".*",
	}
	name := filepath.Base(program)
	flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), `Usage:
  %s --target DIRECTORY [--match REGEX]
  %s --list
  %s --rename-pdfs DIRECTORY

`, name, name, name)
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "\nMaintenance: debug --help")
	}
	flags.StringVar(&config.AppPath, "app", config.AppPath, "Amazon Kindle.app path")
	flags.StringVar(&config.Library, "library", "", "limit automatic search to this Kindle eBooks directory")
	flags.BoolVar(&config.List, "list", false, "automatically find books and list their ASIN/ID")
	flags.StringVar(&config.Match, "match", config.Match, "decrypt only titles/IDs matching this Go regular expression")
	flags.StringVar(&config.RenamePDFs, "rename-pdfs", "", "rename ID-prefixed PDFs in this directory using book titles")
	flags.StringVar(&config.Target, "target", "", "required directory for decrypted books")
	if err := flags.Parse(arguments); err != nil {
		return app.Config{}, err
	}
	if flags.NArg() != 0 {
		return app.Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := validate(config); err != nil {
		return app.Config{}, err
	}
	return config, nil
}

func validate(config app.Config) error {
	if config.List && config.RenamePDFs != "" {
		return errors.New("--list and --rename-pdfs are mutually exclusive")
	}
	if config.List && config.Target != "" {
		return errors.New("--list and --target are mutually exclusive")
	}
	if config.RenamePDFs != "" && config.Target != "" {
		return errors.New("--rename-pdfs and --target are mutually exclusive")
	}
	if !config.List && config.RenamePDFs == "" && config.Target == "" {
		return errors.New("--target is required (or use --list or --rename-pdfs)")
	}
	return nil
}
