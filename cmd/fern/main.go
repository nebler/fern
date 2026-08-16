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
		log.Error("command failed", "err", err)
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "up":
		return runUp(args[1:], log)
	case "attach":
		return runAttach(args[1:])
	case "down":
		return runDown(args[1:], log)
	case "status":
		return runStatus(args[1:], log)
	case "logs":
		return runLogs(args[1:], log)
	case "version":
		return runVersion(args[1:], os.Stdout)
	case "debug":
		if len(args) > 1 && args[1] == "events" {
			return runEvents(args[2:], log)
		}
	}
	return usage()
}

func usage() error {
	fmt.Fprintln(os.Stderr, usageText)
	return errors.New("invalid command")
}

const usageText = "usage: fern <up|attach|down|status|logs|version|debug events> [flags]"
