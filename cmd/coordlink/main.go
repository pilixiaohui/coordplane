package main

import (
	"context"
	"os"

	"coordplane/internal/coordlinkcli"
)

func main() {
	os.Exit(coordlinkcli.Run(context.Background(), os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}
