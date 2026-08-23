package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	config, err := parse("leafport", []string{"--target", "/tmp/books"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Target != "/tmp/books" || config.Match != ".*" {
		t.Fatalf("unexpected config: %+v", config)
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

func TestInvalidFlagReturnsFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"leafport", "--invalid"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "unknown flag") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}
