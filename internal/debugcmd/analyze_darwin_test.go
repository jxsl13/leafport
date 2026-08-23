//go:build darwin

package debugcmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestAnalyzeCurrentExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"analyze", "--binary", executable}, &stdout, &stderr); err != nil {
		t.Fatalf("analyze: %v; stderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "__text SHA-256:") ||
		!strings.Contains(stdout.String(), "Compatibility profile:") ||
		!strings.Contains(stdout.String(), "Credential classes:") ||
		!strings.Contains(stdout.String(), "Credential selectors:") {
		t.Fatalf("analysis output = %q", stdout.String())
	}
}
