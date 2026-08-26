package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := run(os.Args[1:], log); err != nil {
		fmt.Fprintf(os.Stderr, "fern: %v\n", err)
		var invocation invocationError
		if errors.As(err, &invocation) {
			fmt.Fprintln(os.Stderr, "Run 'fern --help' for usage.")
		}
		os.Exit(exitCode(err))
	}
}

func run(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return invocationError{message: "a command is required"}
	}
	switch args[0] {
	case "-h", "--help":
		printTopLevelHelp(os.Stdout)
		return nil
	case "--version":
		return runVersion(nil, os.Stdout)
	case "help":
		return runHelp(args[1:], func(helpArgs []string) error { return run(helpArgs, log) })
	}
	entry := lookupCommand(args[0])
	if entry == nil {
		return unknownCommand(args)
	}
	err := dispatchCommand(entry, args[1:], log)
	if errors.Is(err, errHelpShown) {
		return nil
	}
	return err
}

// dispatchCommand routes remaining arguments to a registry entry. Namespace
// commands (debug, github) resolve their single subcommand level; version and
// the namespaces render grouped help for -h because they do not parse flags.
func dispatchCommand(entry *command, args []string, log *slog.Logger) error {
	ctx := context.Background()
	if entry.overview != "" && len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stdout, groupedHelp(entry))
		return nil
	}
	if entry.run != nil {
		return entry.run(ctx, args, log)
	}
	if len(args) == 0 {
		return unknownCommand([]string{entry.name})
	}
	sub := entry.subcommand(args[0])
	if sub == nil {
		return unknownCommand(append([]string{entry.name}, args...))
	}
	return sub.run(ctx, args[1:], log)
}
