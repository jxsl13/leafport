// Leafport exports locally downloaded books through one module-level command.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"leafport/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr))
}
