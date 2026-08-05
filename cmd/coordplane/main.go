package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"coordplane/internal/operatorcli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	exitCode := operatorcli.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(exitCode)
}
