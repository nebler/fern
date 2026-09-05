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
	"runs":                          "fern runs --endpoint https://fern-host.example.ts.net",
	"attach":                        "fern attach --endpoint https://fern-host.example.ts.net tsk_...",
	"init":                          "fern init --repo /path/to/repository",
	"doctor":                        "fern doctor --phone",
	"up":                            "fern up --config /etc/fern/fern.yaml",
	"debug quarantine-publications": "fern debug quarantine-publications --name demo",
	"backup create":                 "fern backup create --output /srv/backups/fern-generation",
	"backup restore":                "fern backup restore --backup /srv/backups/fern-generation",
	"backup rollback":               "fern backup rollback --recovery-dir /var/lib/fern/recovery",
	"credentials export":            "fern credentials export --recipient age1... --output /srv/backups/credentials.age",
	"credentials import":            "fern credentials import --identity /secure/identity.txt --input credentials.age",
	"credentials rotate":            "fern credentials rotate --identity /secure/identity.txt --input next.age --acknowledge-external-revocation",
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
			fmt.Fprintln(os.Stdout, subcommandUsage(entry))
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
	command := strings.Join(args, " ")
	// Collapse multi-word invocations to the parent command unless the parent
	// is a namespace with several subcommands, where the sub name may be the
	// real typo target.
	entry := lookupCommand(args[0])
	if len(args) > 1 && (entry == nil || len(entry.sub) <= 1) {
		command = args[0]
	}
	message := fmt.Sprintf("unknown command %q", command)
	if suggestion := suggestCommand(command); suggestion != "" {
		message += fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return invocationError{message: message}
}

// suggestionCandidates lists every directly runnable command and every
// two-level "parent sub" pair, in registry order.
func suggestionCandidates() []string {
	names := make([]string, 0, 2*len(commands))
	for _, entry := range commands {
		if entry.run != nil {
			names = append(names, entry.name)
		}
		for _, sub := range entry.sub {
			names = append(names, entry.name+" "+sub.name)
		}
	}
	return names
}

func suggestCommand(input string) string {
	best, distance := "", 3
	for _, candidate := range suggestionCandidates() {
		if current := editDistance(input, candidate); current < distance {
			best, distance = candidate, current
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
