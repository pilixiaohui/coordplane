package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"coordplane/internal/gitcapture"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "capture":
		err = capture(ctx, os.Args[2:])
	case "inspect":
		err = inspect(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func capture(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("coordplane-git-helper capture", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	request := gitcapture.Request{}
	flags.StringVar(&request.Workspace, "workspace", "", "read-only workspace path")
	flags.StringVar(&request.Handoff, "handoff", "", "writable handoff path")
	flags.StringVar(&request.ExpectedHead, "expected-head", "", "expected workspace HEAD (post-commit HEAD, not task base SHA)")
	flags.StringVar(&request.BaseSHA, "base", "", "immutable task base")
	flags.StringVar(&request.SourceSHA, "source", "", "optional immutable source head")
	flags.Int64Var(&request.MaximumBundleBytes, "max-bundle-bytes", 0, "maximum bundle bytes")
	flags.IntVar(&request.MaximumObjects, "max-objects", 0, "maximum reachable objects")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			err = fmt.Errorf("capture does not accept positional arguments")
		}
		return err
	}
	if _, err := gitcapture.Capture(ctx, request); err != nil {
		return err
	}
	return nil
}

func inspect(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("coordplane-git-helper inspect", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	request := gitcapture.InspectRequest{}
	flags.StringVar(&request.Workspace, "workspace", "", "read-only workspace path")
	flags.StringVar(&request.Handoff, "handoff", "", "writable handoff path")
	flags.IntVar(&request.MaximumObjects, "max-objects", 0, "maximum reachable objects")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			err = fmt.Errorf("inspect does not accept positional arguments")
		}
		return err
	}
	_, err := gitcapture.Inspect(ctx, request)
	return err
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: coordplane-git-helper capture|inspect [flags]")
	os.Exit(2)
}
