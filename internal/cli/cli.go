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

	"leafport/internal/app"
	"leafport/internal/debugcmd"
	"leafport/internal/exporter"
	"leafport/internal/kfxconvert"
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
		AppPath:       "/Applications/Amazon Kindle.app",
		Match:         ".*",
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

`, name, name, name)
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "\nDiagnostics: doctor --help\nMaintenance: debug --help")
	}
	flags.StringVar(&config.AppPath, "app", config.AppPath, "Amazon Kindle.app path")
	flags.StringVar(&config.Library, "library", "", "limit automatic search to this Kindle eBooks directory")
	flags.BoolVar(&config.List, "list", false, "automatically find books and list their ASIN/ID")
	flags.StringVar(&config.Match, "match", config.Match, "decrypt only titles/IDs matching this Go regular expression")
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
	if err := kfxconvert.ValidatePrivacyOptions(kfxconvert.PrivacyOptions{
		DetectPersonal: config.RedactPersonal,
		Patterns:       config.RedactPatterns,
	}); err != nil {
		return err
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
