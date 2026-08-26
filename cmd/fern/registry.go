package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// subcommand is a second-level command beneath a namespace command such as
// debug or github.
type subcommand struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, log *slog.Logger) error
}

// command is one top-level fern command. Namespace commands (debug, github)
// carry subcommands instead of a run function; overview prefixes the grouped
// help those namespaces (and flag-less version) print for -h. Usage rows follow
// the historical layout: a command with a summary renders one row for itself; a
// namespace without a summary renders one row per subcommand instead.
// Per-command flag-help examples live in commandExamples (cli.go) because
// deriving them from this table would create a package initialization cycle.
type command struct {
	name     string
	summary  string
	overview string
	sub      []subcommand
	run      func(ctx context.Context, args []string, log *slog.Logger) error
}

// commands is the single source of truth for routing, top-level usage text,
// help dispatch, and typo suggestions.
var commands = []command{
	{
		name: "init", summary: "Create a secure phone-demo configuration",
		run: func(_ context.Context, args []string, _ *slog.Logger) error { return runInit(args) },
	},
	{
		name: "doctor", summary: "Verify host and private phone-demo readiness",
		run: func(_ context.Context, args []string, _ *slog.Logger) error { return runDoctor(args) },
	},
	{
		name: "github", summary: "Validate the retired standalone GitHub preflight",
		overview: "Run the non-mutating legacy GitHub publication preflight.",
		sub: []subcommand{{
			name: "publish",
			run:  func(_ context.Context, args []string, log *slog.Logger) error { return runGitHubPublish(args, log) },
		}},
	},
	{
		name: "up", summary: "Run the workspace supervisor and authenticated proxy",
		run: func(_ context.Context, args []string, log *slog.Logger) error { return runUp(args, log) },
	},
	{
		name: "attach", summary: "Open the official client through the Fern proxy",
		run: func(_ context.Context, args []string, _ *slog.Logger) error { return runAttach(args) },
	},
	{
		name: "status", summary: "Show the workspace runtime state",
		run: func(_ context.Context, args []string, log *slog.Logger) error { return runStatus(args, log) },
	},
	{
		name: "logs", summary: "Stream workspace container logs",
		run: func(_ context.Context, args []string, log *slog.Logger) error { return runLogs(args, log) },
	},
	{
		name: "down", summary: "Remove workspace compute while retaining session data",
		run: func(_ context.Context, args []string, log *slog.Logger) error { return runDown(args, log) },
	},
	{
		name:     "backup",
		overview: "Create, restore, and roll back verified offline host backups.",
		sub: []subcommand{
			{name: "create", summary: "Quiesce the workspace and create a verified backup", run: func(_ context.Context, args []string, log *slog.Logger) error { return runBackupCreate(args, log) }},
			{name: "restore", summary: "Stage, verify, and activate a backup", run: func(_ context.Context, args []string, log *slog.Logger) error { return runBackupRestore(args, log) }},
			{name: "rollback", summary: "Activate the durable pre-restore generation", run: func(_ context.Context, args []string, log *slog.Logger) error { return runBackupRollback(args, log) }},
		},
	},
	{
		name:     "credentials",
		overview: "Export, import, and rollback-safely rotate encrypted GitHub credentials.",
		sub: []subcommand{
			{name: "export", summary: "Export an age-encrypted credential bundle", run: func(_ context.Context, args []string, log *slog.Logger) error { return runCredentialExport(args, log) }},
			{name: "import", summary: "Validate and activate encrypted credentials", run: func(_ context.Context, args []string, log *slog.Logger) error {
				return runCredentialImport(args, log, false)
			}},
			{name: "rotate", summary: "Rotate credentials with an encrypted rollback", run: func(_ context.Context, args []string, log *slog.Logger) error {
				return runCredentialImport(args, log, true)
			}},
		},
	},
	{
		name:     "debug",
		overview: "Inspect Fern internals and run explicit offline repairs.",
		sub: []subcommand{
			{
				name: "events", summary: "Stream the backend activity events used by Fern",
				run: func(_ context.Context, args []string, log *slog.Logger) error { return runEvents(args, log) },
			},
			{
				name: "wake", summary: "Print the phase waterfall for one workspace wake",
				run: func(_ context.Context, args []string, log *slog.Logger) error { return runDebugWake(args, log) },
			},
			{
				name: "quarantine-publications", summary: "Quarantine unresolved retired publication records",
				run: func(_ context.Context, args []string, _ *slog.Logger) error {
					return runLegacyPublicationQuarantine(args, os.Stdout)
				},
			},
		},
	},
	{
		name: "version", summary: "Print Fern version information",
		overview: "Print Fern version information.",
		run:      func(_ context.Context, args []string, _ *slog.Logger) error { return runVersion(args, os.Stdout) },
	},
}

func lookupCommand(name string) *command {
	for index := range commands {
		if commands[index].name == name {
			return &commands[index]
		}
	}
	return nil
}

func (c *command) subcommand(name string) *subcommand {
	for index := range c.sub {
		if c.sub[index].name == name {
			return &c.sub[index]
		}
	}
	return nil
}

func lookupSubcommand(parent, name string) *subcommand {
	entry := lookupCommand(parent)
	if entry == nil {
		return nil
	}
	return entry.subcommand(name)
}

// groupedHelp renders the short help printed for 'fern <namespace> --help':
// an overview line plus either the version form or one usage line per
// subcommand.
func groupedHelp(entry *command) string {
	lines := []string{entry.overview, "", "Usage:"}
	if len(entry.sub) == 0 {
		lines = append(lines, "  fern "+entry.name)
	}
	for _, sub := range entry.sub {
		lines = append(lines, fmt.Sprintf("  fern %s %s [flags]", entry.name, sub.name))
	}
	return strings.Join(lines, "\n")
}

// subcommandUsage renders the bare usage block printed for
// 'fern help <namespace>'.
func subcommandUsage(entry *command) string {
	lines := []string{"Usage:"}
	for _, sub := range entry.sub {
		lines = append(lines, fmt.Sprintf("  fern %s %s [flags]", entry.name, sub.name))
	}
	return strings.Join(lines, "\n")
}

// usageExamples are the curated top-level examples shown in 'fern --help'.
var usageExamples = []string{
	"fern init --repo /path/to/repository",
	"fern up --config fern.yaml",
	"fern status --json",
	"fern attach",
}

// buildUsageText derives top-level help from the registry so commands appear,
// disappear, and align without hand-editing the text.
func buildUsageText() string {
	type row struct{ name, summary string }
	var rows []row
	width := 0
	appendRow := func(name, summary string) {
		rows = append(rows, row{name: name, summary: summary})
		if len(name) > width {
			width = len(name)
		}
	}
	for _, entry := range commands {
		// Historical layout: a summary renders one row for the command itself;
		// a namespace without a summary renders one row per subcommand.
		if entry.summary != "" {
			appendRow(entry.name, entry.summary)
		}
		if entry.summary == "" {
			for _, sub := range entry.sub {
				appendRow(entry.name+" "+sub.name, sub.summary)
			}
		}
	}
	var builder strings.Builder
	builder.WriteString("Fern supervises one durable OpenCode workspace in Docker.\n\nUsage:\n  fern <command> [flags]\n  fern help [command]\n\nCommands:\n")
	for _, entry := range rows {
		fmt.Fprintf(&builder, "  %-*s  %s\n", width, entry.name, entry.summary)
	}
	builder.WriteString("\nExamples:\n")
	for _, example := range usageExamples {
		fmt.Fprintf(&builder, "  %s\n", example)
	}
	builder.WriteString("\nRun 'fern help <command>' for command flags.\nDocumentation: https://github.com/nebler/fern\n")
	return builder.String()
}

var usageText = buildUsageText()
