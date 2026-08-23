package debugcmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/pflag"

	"leafport/internal/readerconfig"
)

func runDebugCredentials(arguments []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("debug credentials", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	preferences := flags.String("preferences", "", "reader preferences plist")
	var roots []string
	flags.StringSliceVar(&roots, "root", nil, "reader-owned storage root; repeatable")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: leafport debug credentials --preferences PATH [--root PATH]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *preferences == "" {
		return errors.New("debug credentials requires --preferences PATH")
	}
	credentials, err := readerconfig.LoadCredentials(*preferences)
	if err != nil {
		return fmt.Errorf("debug credentials: %w", err)
	}
	if credentials.HashedAccountSecret == "" {
		return errors.New("debug credentials: preferences contain no hashed account secret")
	}
	if len(roots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("debug credentials: %w", err)
		}
		roots = readerconfig.DefaultAccountSecretRoots(home)
	}
	secret, report, err := readerconfig.DiscoverAccountSecret(credentials.HashedAccountSecret, roots)
	if err != nil {
		return fmt.Errorf("debug credentials: %w", err)
	}
	fmt.Fprintf(stdout, "Files scanned: %d\nBytes scanned: %d\n40-hex candidates tested: %d\nUnreadable entries: %d\nVerified account secret found: %t\n",
		report.Files, report.Bytes, report.Candidates, report.Unreadable, secret != "")
	return nil
}
