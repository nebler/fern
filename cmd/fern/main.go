package main

import (
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
	if args[0] == "-h" || args[0] == "--help" {
		printTopLevelHelp(os.Stdout)
		return nil
	}
	if args[0] == "--version" {
		return runVersion(nil, os.Stdout)
	}
	if args[0] == "help" {
		return runHelp(args[1:], func(helpArgs []string) error { return run(helpArgs, log) })
	}
	var err error
	switch args[0] {
	case "init":
		err = runInit(args[1:])
	case "doctor":
		err = runDoctor(args[1:])
	case "github":
		err = runGitHub(args[1:])
	case "up":
		err = runUp(args[1:], log)
	case "attach":
		err = runAttach(args[1:])
	case "down":
		err = runDown(args[1:], log)
	case "status":
		err = runStatus(args[1:], log)
	case "logs":
		err = runLogs(args[1:], log)
	case "version":
		if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
			fmt.Fprintln(os.Stdout, "Print Fern version information.\n\nUsage:\n  fern version")
			return nil
		}
		err = runVersion(args[1:], os.Stdout)
	case "debug":
		if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
			fmt.Fprintln(os.Stdout, "Inspect Fern's backend activity inputs.\n\nUsage:\n  fern debug events [flags]")
			return nil
		}
		if len(args) > 1 && args[1] == "events" {
			err = runEvents(args[2:], log)
			break
		}
		return unknownCommand(args)
	default:
		return unknownCommand(args)
	}
	if errors.Is(err, errHelpShown) {
		return nil
	}
	return err
}
