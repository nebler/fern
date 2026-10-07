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
		fmt.Fprintf(output, "%s\n\nUsage:\n  fern %s [flags]%s\n", description, command, commandUsageSuffix[command])
		fmt.Fprintln(output, "\nFlags:")
		flags.PrintDefaults()
		if example := commandExamples[command]; example != "" {
			fmt.Fprintf(output, "\nExample:\n  %s\n", example)
		}
	}
	return flags
}

var commandUsageSuffix = map[string]string{"attach": " [run-id]"}

// commandExamples holds the Example line shown in each command's flag help,
// keyed the way flag help addresses commands ("name" or "parent sub"). It stays
// a plain literal because deriving it from the command registry would create a
// package initialization cycle.
var commandExamples = map[string]string{
	"runs":            "fern runs --endpoint https://fern-host.example.ts.net",
	"attach":          "fern attach --endpoint https://fern-host.example.ts.net run_...",
	"init":            "fern init --repo /path/to/repository",
	"doctor":          "fern doctor --phone",
	"up":              "fern up --config /etc/fern/fern.yaml",
	"backup create":   "fern backup create --recipient age1... --output /srv/backups/fern.backup",
	"backup restore":  "fern backup restore --identity /secure/identity.txt --input /srv/backups/fern.backup",
	"credentials set": "fern credentials set --app-id 123456 --private-key /secure/fern-app.pem",
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := parseFlagValues(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return invocationError{message: fmt.Sprintf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	return nil
}

func parseFlagValues(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return errHelpShown
		}
		return invocationError{message: err.Error()}
	}
	return nil
}

func printTopLevelHelp(output io.Writer) {
	fmt.Fprint(output, usageText)
}

func runHelp(args []string, dispatch func([]string) error) error {
	if len(args) == 0 {
		printTopLevelHelp(os.Stdout)
		return nil
	}
	if len(args) == 1 {
		if entry := lookupCommand(args[0]); entry != nil && entry.run == nil && len(entry.sub) > 0 {
			fmt.Fprintln(os.Stdout, groupedHelp(entry))
			return nil
		}
	}
	if len(args) > 2 || (len(args) == 2 && lookupSubcommand(args[0], args[1]) == nil) {
		return invocationError{message: "usage: fern help [command]"}
	}
	helpArgs := append([]string(nil), args...)
	return dispatch(append(helpArgs, "--help"))
}

func unknownCommand(args []string) error {
	return invocationError{message: fmt.Sprintf("unknown command %q", strings.Join(args, " "))}
}
