package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

var errHelpShown = errors.New("help shown")

type invocationError struct {
	message string
}

func (e invocationError) Error() string { return e.message }

type commandExitError struct {
	err  error
	code int
}

func (e commandExitError) Error() string { return e.err.Error() }
func (e commandExitError) Unwrap() error { return e.err }

func exitCode(err error) int {
	var invocation invocationError
	if errors.As(err, &invocation) {
		return 2
	}
	var command commandExitError
	if errors.As(err, &command) && command.code > 0 && command.code <= 255 {
		return command.code
	}
	return 1
}

func newFlagSet(command, description string) *flag.FlagSet {
	flags := flag.NewFlagSet("fern "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		output := flags.Output()
		fmt.Fprintf(output, "%s\n\nUsage:\n  fern %s [flags]\n", description, command)
		fmt.Fprintln(output, "\nFlags:")
		flags.PrintDefaults()
		if example := commandExamples[command]; example != "" {
			fmt.Fprintf(output, "\nExample:\n  %s\n", example)
		}
	}
	return flags
}

var commandExamples = map[string]string{
	"init":           "fern init --repo /path/to/repository",
	"doctor":         "fern doctor --phone",
	"github publish": "fern github publish --title 'Describe the change'",
	"up":             "fern up --config /etc/fern/fern.yaml",
	"attach":         "fern attach --url https://host.tailnet.ts.net",
	"down":           "fern down --name demo",
	"status":         "fern status --name demo --json",
	"logs":           "fern logs --name demo --follow=false",
	"debug events":   "fern debug events --name demo",
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(os.Stdout)
			flags.Usage()
			return errHelpShown
		}
		return invocationError{message: err.Error()}
	}
	if flags.NArg() != 0 {
		return invocationError{message: fmt.Sprintf("unexpected arguments: %s", strings.Join(flags.Args(), " "))}
	}
	return nil
}

func printTopLevelHelp(output io.Writer) {
	fmt.Fprint(output, usageText)
}

const usageText = `Fern supervises one durable OpenCode workspace in Docker.

Usage:
  fern <command> [flags]
  fern help [command]

Commands:
  init          Create a secure phone-demo configuration
  doctor        Verify host and private phone-demo readiness
  github        Publish committed work as a draft pull request
  up            Run the workspace supervisor and authenticated proxy
  attach        Open the official client through the Fern proxy
  status        Show the workspace runtime state
  logs          Stream workspace container logs
  down          Remove workspace compute while retaining session data
  debug events  Stream the backend activity events used by Fern
  version       Print Fern version information

Examples:
  fern init --repo /path/to/repository
  fern up --config fern.yaml
  fern status --json
  fern attach

Run 'fern help <command>' for command flags.
Documentation: https://github.com/nebler/fern
`

func runHelp(args []string, dispatch func([]string) error) error {
	if len(args) == 0 {
		printTopLevelHelp(os.Stdout)
		return nil
	}
	if len(args) == 1 && (args[0] == "debug" || args[0] == "github") {
		if args[0] == "debug" {
			fmt.Fprintln(os.Stdout, "Usage:\n  fern debug events [flags]")
		} else {
			fmt.Fprintln(os.Stdout, "Usage:\n  fern github publish [flags]")
		}
		return nil
	}
	if len(args) > 2 || len(args) == 2 && !((args[0] == "debug" && args[1] == "events") || (args[0] == "github" && args[1] == "publish")) {
		return invocationError{message: "usage: fern help [command]"}
	}
	helpArgs := append([]string(nil), args...)
	return dispatch(append(helpArgs, "--help"))
}

func unknownCommand(args []string) error {
	command := strings.Join(args, " ")
	if len(args) > 1 && args[0] != "debug" {
		command = args[0]
	}
	message := fmt.Sprintf("unknown command %q", command)
	if suggestion := suggestCommand(command); suggestion != "" {
		message += fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return invocationError{message: message}
}

func suggestCommand(input string) string {
	commands := []string{"init", "doctor", "github publish", "up", "attach", "down", "status", "logs", "version", "debug events"}
	best, distance := "", 3
	for _, command := range commands {
		if current := editDistance(input, command); current < distance {
			best, distance = command, current
		}
	}
	return best
}

func editDistance(left, right string) int {
	leftRunes, rightRunes := []rune(left), []rune(right)
	previous := make([]int, len(rightRunes)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftRune := range leftRunes {
		current := make([]int, len(rightRunes)+1)
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range rightRunes {
			cost := 1
			if leftRune == rightRune {
				cost = 0
			}
			current[rightIndex+1] = min(current[rightIndex]+1, previous[rightIndex+1]+1, previous[rightIndex]+cost)
		}
		previous = current
	}
	return previous[len(rightRunes)]
}
