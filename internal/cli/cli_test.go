package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	t.Setenv("LEAFPORT_ACCOUNT_SECRET", "")
	config, err := parse("leafport", []string{"--target", "/tmp/books"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Target != "/tmp/books" || config.Match != ".*" {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestParseExplicitFormat(t *testing.T) {
	config, err := parse("leafport", []string{"--target", "/tmp/books", "--format", "comic-epub"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Format != "comic-epub" {
		t.Fatalf("format = %q", config.Format)
	}
	if _, err := parse("leafport", []string{"--target", "/tmp/books", "--format", "mobi"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unsupported format was accepted")
	}
}

func TestParseDebugArtifacts(t *testing.T) {
	config, err := parse("leafport", []string{"--target", "/tmp/books", "--debug"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Debug {
		t.Fatal("--debug was not enabled")
	}
	if _, err := parse("leafport", []string{"--list", "--debug"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--debug without --target was accepted")
	}
}

func TestParsePrivacyFlags(t *testing.T) {
	config, err := parse("leafport", []string{
		"--target", "/tmp/books", "--redact-personal", "--redact", `(?i)jane@example\.com`, "--redact", "Jane Doe",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !config.RedactPersonal || len(config.RedactPatterns) != 2 || config.PreparedPrivacy == nil {
		t.Fatalf("config = %+v", config)
	}
	for _, arguments := range [][]string{
		{"--list", "--redact-personal"},
		{"--target", "/tmp/books", "--redact", "("},
		{"--target", "/tmp/books", "--redact", ".*"},
	} {
		if _, err := parse("leafport", arguments, &bytes.Buffer{}); err == nil {
			t.Fatalf("arguments %q were accepted", arguments)
		}
	}
}

func TestParseAccountSecretFromEnvironment(t *testing.T) {
	t.Setenv("LEAFPORT_ACCOUNT_SECRET", strings.Repeat("s", 40))
	config, err := parse("leafport", []string{"--target", "/tmp/books"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.AccountSecret) != 40 {
		t.Fatalf("account-secret length = %d", len(config.AccountSecret))
	}
	t.Setenv("LEAFPORT_ACCOUNT_SECRET", "too-short")
	if _, err := parse("leafport", []string{"--target", "/tmp/books"}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid account secret was accepted")
	}
}

func TestParseRejectsPositionalArguments(t *testing.T) {
	if _, err := parse("leafport", []string{"unexpected"}, &bytes.Buffer{}); err == nil {
		t.Fatal("positional argument was accepted")
	}
}

func TestParseRequiresOneOperation(t *testing.T) {
	if _, err := parse("leafport", nil, &bytes.Buffer{}); err == nil {
		t.Fatal("missing operation was accepted")
	}
	if _, err := parse("leafport", []string{"--list", "--target", "/tmp/books"}, &bytes.Buffer{}); err == nil {
		t.Fatal("conflicting operations were accepted")
	}
}

func TestParseRejectsRemovedRenamePDFsFlag(t *testing.T) {
	if _, err := parse("leafport", []string{"--rename-pdfs", "/tmp/books"}, &bytes.Buffer{}); err == nil {
		t.Fatal("removed --rename-pdfs flag was accepted")
	}
}

func TestParseListOutputWide(t *testing.T) {
	config, err := parse("leafport", []string{"--list", "-o", "wide"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !config.List || config.Output != "wide" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if _, err := parse("leafport", []string{"--target", "/tmp/books", "-o", "wide"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--output without --list was accepted")
	}
	if _, err := parse("leafport", []string{"--list", "--output", "json"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unsupported output format was accepted")
	}
}

func TestDebugHelpUsesInjectedStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"leafport", "debug", "--help"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "debug disasm") {
		t.Fatalf("help output = %q", stdout.String())
	}
}

func TestDoctorHelpUsesInjectedStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"leafport", "doctor", "--help"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "leafport doctor") {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestPublicationCommandHelpAndNoOverwriteGuard(t *testing.T) {
	for _, command := range []string{"validate", "fix"} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"leafport", command, "--help"},
			strings.NewReader(""), &stdout, &stderr)
		if code != 0 || !strings.Contains(stderr.String(), "Usage:") {
			t.Fatalf("%s help: code=%d stderr=%q", command, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{
		"leafport", "fix", "--input", "/tmp/original.epub", "--output", "/tmp/original.epub",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "originals are never modified") {
		t.Fatalf("same-path fix: code=%d stderr=%q", code, stderr.String())
	}
}

func TestInvalidFlagReturnsFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"leafport", "--invalid"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "unknown flag") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}

func TestParseDoctorUsesLongFlags(t *testing.T) {
	config, err := parseDoctor("leafport", []string{"--app", "/Applications/Reader.app", "--library", "/tmp/books"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.AppPath != "/Applications/Reader.app" || config.Library != "/tmp/books" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if _, err := parseDoctor("leafport", []string{"unexpected"}, &bytes.Buffer{}); err == nil {
		t.Fatal("doctor positional argument was accepted")
	}
}
