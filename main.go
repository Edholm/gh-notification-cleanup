// Command gh-notification-cleanup is a GitHub CLI extension that marks notifications as done for pull
// requests that have been closed or merged, and for CI failures.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/Edholm/gh-notification-cleanup/internal/cleanup"
	"github.com/cli/go-gh/v2/pkg/api"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var opts cleanup.Options
	flags := flag.NewFlagSet("gh notification-cleanup", flag.ExitOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage: %s [flags]\n\n", flags.Name())
		_, _ = fmt.Fprintln(flags.Output(), "Marks notifications as done for closed or merged pull requests and CI failures.")
		_, _ = fmt.Fprintln(flags.Output(), "Without --merged, --closed or --ci, all three are cleaned up.")
		_, _ = fmt.Fprintln(flags.Output())
		flags.PrintDefaults()
	}
	flags.BoolVar(&opts.Merged, "merged", false, "clean up notifications for merged pull requests")
	flags.BoolVar(&opts.Closed, "closed", false, "clean up notifications for pull requests closed without merging")
	flags.BoolVar(&opts.CIFailure, "ci", false, "clean up notifications for failed CI runs")
	flags.BoolVar(&opts.DryRun, "dry-run", false, "only print what would be marked as done")
	flags.BoolVar(&opts.DryRun, "n", false, "shorthand for --dry-run")
	verbose := flags.Bool("verbose", false, "log debug output")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if !opts.Merged && !opts.Closed && !opts.CIFailure {
		opts.Merged, opts.Closed, opts.CIFailure = true, true, true
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := api.NewRESTClient(api.ClientOptions{})
	if err != nil {
		return fmt.Errorf("create GitHub client (is gh authenticated?): %w", err)
	}
	summary, err := cleanup.New(client, logger, os.Stdout, opts).Run(ctx)
	_, _ = fmt.Fprintf(os.Stderr, "%d of %d notifications cleaned up\n", summary.Cleaned, summary.Inspected)
	return err
}
