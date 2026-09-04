// Package cli parses Leafport commands and maps failures to process exit codes.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/jxsl13/leafport/internal/app"
	"github.com/jxsl13/leafport/internal/debugcmd"
	"github.com/jxsl13/leafport/internal/exporter"
	"github.com/jxsl13/leafport/internal/kfxconvert"
)

// Run executes Leafport with explicit arguments and streams.
func Run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := run(ctx, arguments, stdin, stdout, stderr); err != nil {
		reported, alreadyReported := err.(interface{ AlreadyReported() bool })
		if !alreadyReported || !reported.AlreadyReported() {
			fmt.Fprintf(stderr, "leafport: %v\n", err)
		}
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
		case exporter.DoctorRuntimeCommand:
			return exporter.RunDoctorRuntime(arguments[2:], stdout)
		case "doctor":
			config, err := parseDoctor(arguments[0], arguments[2:], stderr)
			if errors.Is(err, pflag.ErrHelp) {
				return nil
			}
			if err != nil {
				return err
			}
			return app.Doctor(ctx, config, app.Streams{Stdin: stdin, Stdout: stdout, Stderr: stderr})
		case "debug":
			return debugcmd.Run(ctx, arguments[2:], stdout, stderr)
		case "validate":
			return runPublicationValidate(arguments[0], arguments[2:], stdout, stderr)
		case "fix":
			return runPublicationFix(arguments[0], arguments[2:], stdout, stderr)
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

func runPublicationValidate(program string, arguments []string, stdout, stderr io.Writer) error {
	var input string
	flags := pflag.NewFlagSet(filepath.Base(program)+" validate", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s validate --input BOOK.{pdf,epub,cbz}\n\n", filepath.Base(program))
		flags.PrintDefaults()
	}
	flags.StringVarP(&input, "input", "i", "", "existing publication to validate")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || input == "" {
		return errors.New("validate requires --input and accepts no positional arguments")
	}
	result, err := kfxconvert.ValidatePublication(input)
	if err != nil {
		return err
	}
	printPublicationSummary(stdout, "valid", input, result)
	return nil
}

func runPublicationFix(program string, arguments []string, stdout, stderr io.Writer) error {
	var input, output string
	flags := pflag.NewFlagSet(filepath.Base(program)+" fix", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s fix --input ORIGINAL --output COPY\n\n", filepath.Base(program))
		flags.PrintDefaults()
	}
	flags.StringVarP(&input, "input", "i", "", "existing PDF, EPUB, or CBZ")
	flags.StringVarP(&output, "output", "o", "", "new normalized copy (must not exist)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || input == "" || output == "" {
		return errors.New("fix requires --input and --output and accepts no positional arguments")
	}
	result, err := kfxconvert.FixPublication(input, output)
	if err != nil {
		return err
	}
	printPublicationSummary(stdout, "created", output, result)
	return nil
}

func printPublicationSummary(output io.Writer, action, path string, result kfxconvert.PublicationValidationResult) {
	details := ""
	switch result.Format {
	case "PDF":
		details = fmt.Sprintf(", %d pages", result.Pages)
	case "EPUB":
		details = fmt.Sprintf(", %d spine items, %d images, %dx%d cover", result.SpineItems, result.Images,
			result.CoverWidth, result.CoverHeight)
	case "CBZ":
		details = fmt.Sprintf(", %d decoded pages, ComicInfo.xml", result.Pages)
	}
	fmt.Fprintf(output, "%s %s: %s%s\n", action, result.Format, path, details)
}

func parse(program string, arguments []string, output io.Writer) (app.Config, error) {
	config := app.Config{
		AppPath:       "/Applications/Amazon Kindle.app",
		Match:         ".*",
		Format:        "auto",
		AccountSecret: strings.TrimSpace(os.Getenv("LEAFPORT_ACCOUNT_SECRET")),
	}
	name := filepath.Base(program)
	flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), `Usage:
  %s --target DIRECTORY [--match REGEX] [--redact-personal] [--redact REGEX]
  %s --list [-o wide]
  %s doctor [--app PATH] [--library PATH]
  %s validate --input BOOK.{pdf,epub,cbz}
  %s fix --input ORIGINAL --output COPY

`, name, name, name, name, name)
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "\nDiagnostics: doctor --help\nMaintenance: debug --help")
	}
	flags.StringVar(&config.AppPath, "app", config.AppPath, "Amazon Kindle.app path")
	flags.StringVar(&config.Library, "library", "", "limit automatic search to this Kindle eBooks directory")
	flags.BoolVar(&config.List, "list", false, "automatically find books and list their ASIN/ID")
	flags.StringVar(&config.Match, "match", config.Match, "decrypt only titles/IDs matching this Go regular expression")
	flags.StringVar(&config.Format, "format", config.Format, "final format: auto, pdf, epub, cbz, or comic-epub")
	flags.StringVarP(&config.Output, "output", "o", "", "output format for --list (wide)")
	flags.StringVar(&config.Target, "target", "", "required directory for decrypted books")
	flags.BoolVar(&config.Debug, "debug", false, "retain encrypted bundles and decrypted KFX archives below TARGET/debug")
	flags.BoolVar(&config.RedactPersonal, "redact-personal", false, "detect owner metadata and remove personal information")
	flags.StringArrayVar(&config.RedactPatterns, "redact", nil, "redact text and metadata matching this Go regular expression (repeatable)")
	if err := flags.Parse(arguments); err != nil {
		return app.Config{}, err
	}
	if flags.NArg() != 0 {
		return app.Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := validate(config); err != nil {
		return app.Config{}, err
	}
	privacy, err := kfxconvert.PreparePrivacyOptions(kfxconvert.PrivacyOptions{
		DetectPersonal: config.RedactPersonal,
		Patterns:       config.RedactPatterns,
	})
	if err != nil {
		return app.Config{}, err
	}
	config.PreparedPrivacy = &privacy
	return config, nil
}

func parseDoctor(program string, arguments []string, output io.Writer) (app.Config, error) {
	config := app.Config{
		AppPath:       "/Applications/Amazon Kindle.app",
		AccountSecret: strings.TrimSpace(os.Getenv("LEAFPORT_ACCOUNT_SECRET")),
	}
	flags := pflag.NewFlagSet(filepath.Base(program)+" doctor", pflag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s doctor [--app PATH] [--library PATH]\n\n", filepath.Base(program))
		flags.PrintDefaults()
	}
	flags.StringVar(&config.AppPath, "app", config.AppPath, "Amazon Kindle.app path")
	flags.StringVar(&config.Library, "library", "", "limit automatic search to this reader eBooks directory")
	if err := flags.Parse(arguments); err != nil {
		return app.Config{}, err
	}
	if flags.NArg() != 0 {
		return app.Config{}, fmt.Errorf("doctor: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := validateAccountSecret(config.AccountSecret); err != nil {
		return app.Config{}, err
	}
	return config, nil
}

func validate(config app.Config) error {
	if err := validateAccountSecret(config.AccountSecret); err != nil {
		return err
	}
	if config.Output != "" && !config.List {
		return errors.New("--output is only valid with --list")
	}
	switch config.Format {
	case "auto", "pdf", "epub", "cbz", "comic-epub":
	default:
		return fmt.Errorf("unsupported --format %q (supported: auto, pdf, epub, cbz, comic-epub)", config.Format)
	}
	if config.List && config.Format != "auto" {
		return errors.New("--format is only valid with --target")
	}
	if config.Output != "" && config.Output != "wide" {
		return fmt.Errorf("unsupported --output format %q (supported: wide)", config.Output)
	}
	if config.List && config.Target != "" {
		return errors.New("--list and --target are mutually exclusive")
	}
	if config.List && config.Debug {
		return errors.New("--debug is only valid with --target")
	}
	if config.List && (config.RedactPersonal || len(config.RedactPatterns) != 0) {
		return errors.New("--redact-personal and --redact are only valid with --target")
	}
	if !config.List && config.Target == "" {
		return errors.New("--target is required (or use --list)")
	}
	return nil
}

func validateAccountSecret(secret string) error {
	if secret != "" && len(secret) != 40 {
		return fmt.Errorf("LEAFPORT_ACCOUNT_SECRET must contain exactly 40 characters; got %d", len(secret))
	}
	return nil
}
