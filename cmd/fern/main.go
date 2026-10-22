// Command fern is the Fern daemon and its command-line client.
//
//	fern daemon [--box-image IMAGE]          run the node daemon and CSI plugin
//	fern create [--path P] [--seed] <env>    make an environment; P is its workspace,
//	                                         --seed fills it from a tar stream on stdin
//	fern ls                                  list environments
//	fern which <path>                        print the environment whose workspace holds path
//	fern rm <env>                            delete an environment
//	fern snapshot [--replace] <env> <snap>   take a snapshot (again, with --replace)
//	fern snapshots <env>                     list snapshots
//	fern rmsnap <env> <snap>…                delete snapshots
//	fern restore <env> <snap>                restore a snapshot; the box stays asleep
//	fern exec [-i|-it] [-e NAME]… <env> -- cmd…
//	                                         run a command, waking the box if needed
//	fern exec [flags] --here -- cmd…         … in the environment holding the current
//	                                         directory, in that directory (fern-claude)
//	fern sleep <env> | fern wake <env>       stop or start the box
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/boxexec"
)

// socket is in a directory of its own, so pods can share it through a
// hostPath.
const socket = "/run/fern/fern.sock"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run runs one invocation and returns the process exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "daemon" {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemon(ctx, args[1:], stderr)
	}
	cmd, err := parse(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fern: %v\n", err)
		return 2
	}
	// exec passes signals on to the command; everything else stops.
	if cmd.name != "exec" {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	code, err := lookup(cmd.name).run(ctx, cmd, api.NewClient(socket), stdio{stdin, stdout, stderr})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fern %s: %v\n", cmd.name, err)
		return 1
	}
	return code
}

// command is a parsed CLI invocation.
type command struct {
	name    string
	args    []string // positional arguments; for exec, the environment
	path    string   // create --path
	seed    bool     // create --seed
	replace bool     // snapshot --replace
	stdin   bool     // exec -i
	tty     bool     // exec -it
	here    bool     // exec --here
	environ []string // exec -e, as NAME=VALUE
	argv    []string // exec: the command
}

// stdio is the invocation's standard streams.
type stdio struct {
	in       io.Reader
	out, err io.Writer
}

// subcommand is one of the CLI's commands.
type subcommand struct {
	name     string
	min, max int // how many positional arguments it takes; max -1 means no limit
	// flags defines the command's flags on fs, to be parsed into c.
	flags func(fs *flag.FlagSet, c *command)
	run   func(ctx context.Context, c command, cl *api.Client, std stdio) (int, error)
}

// subcommands are the CLI's commands, in the order the usage lists them.
// exec parses its own arguments.
func subcommands() []subcommand {
	return []subcommand{
		{name: "create", min: 1, max: 1, run: create, flags: func(fs *flag.FlagSet, c *command) {
			fs.StringVar(&c.path, "path", "", "the workspace: <pool>/worktrees/<repo>/<name>; a directory there is adopted")
			fs.BoolVar(&c.seed, "seed", false, "fill the workspace from a tar stream on stdin")
		}},
		{name: "ls", run: list},
		{name: "which", min: 1, max: 1, run: which},
		{name: "rm", min: 1, max: 1, run: remove},
		{name: "snapshot", min: 2, max: 2, run: snapshot, flags: func(fs *flag.FlagSet, c *command) {
			fs.BoolVar(&c.replace, "replace", false, "take it again if it exists")
		}},
		{name: "snapshots", min: 1, max: 1, run: snapshots},
		{name: "rmsnap", min: 2, max: -1, run: removeSnapshots},
		{name: "restore", min: 2, max: 2, run: restore},
		{name: "exec", run: execute},
		{name: "sleep", min: 1, max: 1, run: timed((*api.Client).Sleep)},
		{name: "wake", min: 1, max: 1, run: timed((*api.Client).Wake)},
	}
}

// lookup returns the subcommand called name; parse has checked it exists.
func lookup(name string) subcommand {
	i := slices.IndexFunc(subcommands(), func(sc subcommand) bool { return sc.name == name })
	return subcommands()[i]
}

// parse turns the CLI's arguments into a command.
func parse(args []string) (command, error) {
	var names []string
	for _, sc := range subcommands() {
		names = append(names, sc.name)
	}
	if len(args) == 0 {
		return command{}, fmt.Errorf("usage: fern <%s> …", strings.Join(names, "|"))
	}
	cmd := command{name: args[0]}
	if !slices.Contains(names, cmd.name) {
		return command{}, fmt.Errorf("unknown command %q", cmd.name)
	}
	sc := lookup(cmd.name)
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if cmd.name == "exec" {
		return parseExec(fs, cmd, args[1:])
	}
	if sc.flags != nil {
		sc.flags(fs, &cmd)
	}
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return command{}, err
	}
	if len(pos) < sc.min || (sc.max >= 0 && len(pos) > sc.max) {
		return command{}, fmt.Errorf("%s: wrong number of arguments", cmd.name)
	}
	cmd.args = pos
	return cmd, nil
}

// parseExec parses exec's flags, which come before the environment:
// everything after it belongs to the command.
func parseExec(fs *flag.FlagSet, cmd command, args []string) (command, error) {
	fs.BoolVar(&cmd.stdin, "i", false, "send stdin")
	fs.BoolVar(&cmd.tty, "it", false, "send stdin from a terminal")
	fs.BoolVar(&cmd.here, "here", false, "use the environment holding the current directory, and run in it")
	fs.Func("e", "pass the variable `NAME` on to the command, if it is set", func(name string) error {
		if v, ok := os.LookupEnv(name); ok {
			cmd.environ = append(cmd.environ, name+"="+v)
		}
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return command{}, err
	}
	pos := fs.Args()
	if !cmd.here {
		if len(pos) == 0 {
			return command{}, errors.New("exec: wrong number of arguments")
		}
		cmd.args, pos = pos[:1], pos[1:]
	}
	cmd.stdin = cmd.stdin || cmd.tty
	if len(pos) > 0 && pos[0] == "--" {
		pos = pos[1:]
	}
	if len(pos) == 0 {
		return command{}, errors.New("exec: no command given")
	}
	cmd.argv = pos
	return cmd, nil
}

// parseInterleaved parses flags that may come before, between or after the
// positional arguments, and returns the positional ones.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func create(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	var seed io.Reader
	if c.seed {
		seed = std.in
	}
	if err := cl.Create(ctx, c.args[0], c.path, seed); err != nil {
		return 0, err
	}
	_, err := fmt.Fprintf(std.out, "created environment %s\n", c.args[0])
	return 0, err
}

func list(ctx context.Context, _ command, cl *api.Client, std stdio) (int, error) {
	envs, err := cl.List(ctx)
	if err != nil {
		return 0, err
	}
	tw := tabwriter.NewWriter(std.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ENV\tSTATE\tCREATED\tWORKSPACE") // tabwriter reports errors on Flush
	for _, e := range envs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, e.State, e.Created.Format(time.RFC3339), e.Workspace)
	}
	return 0, tw.Flush()
}

func which(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	env, err := cl.FindByPath(ctx, c.args[0])
	if err != nil {
		return 0, err
	}
	_, err = fmt.Fprintln(std.out, env.ID)
	return 0, err
}

func remove(ctx context.Context, c command, cl *api.Client, _ stdio) (int, error) {
	return 0, cl.Delete(ctx, c.args[0])
}

func snapshot(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	id, snapID := c.args[0], c.args[1]
	start := time.Now()
	if err := cl.Snapshot(ctx, id, snapID, c.replace); err != nil {
		return 0, err
	}
	return 0, took(std, start, "snapshot "+id+"/"+snapID)
}

func snapshots(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	snaps, err := cl.Snapshots(ctx, c.args[0])
	if err != nil {
		return 0, err
	}
	tw := tabwriter.NewWriter(std.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SNAPSHOT\tCREATED")
	for _, s := range snaps {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", s.ID, s.Created.Format(time.RFC3339))
	}
	return 0, tw.Flush()
}

func removeSnapshots(ctx context.Context, c command, cl *api.Client, _ stdio) (int, error) {
	for _, snapID := range c.args[1:] {
		if err := cl.DeleteSnapshot(ctx, c.args[0], snapID); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func restore(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	id, snapID := c.args[0], c.args[1]
	start := time.Now()
	if err := cl.Restore(ctx, id, snapID); err != nil {
		return 0, err
	}
	return 0, took(std, start, "restore "+id+" to "+snapID)
}

func execute(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	req := boxexec.Request{Argv: c.argv, TTY: c.tty, Environ: c.environ}
	var id string
	if c.here {
		wd, err := os.Getwd()
		if err != nil {
			return 0, err
		}
		env, err := cl.FindByPath(ctx, wd)
		if err != nil {
			return 0, err
		}
		id, req.Dir = env.ID, wd
	} else {
		id = c.args[0]
	}
	streams := boxexec.Streams{Stdout: std.out, Stderr: std.err}
	if c.stdin {
		streams.Stdin = std.in
	}
	var relays sync.WaitGroup
	defer relays.Wait() // after cancel, which runs first
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Signals go to the command, which decides how to stop (T3 stops Claude
	// with SIGTERM); a second interrupt gives up on it.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	streams.Signal = sigs
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	relays.Go(func() {
		for range 2 {
			select {
			case <-ctx.Done():
				return
			case <-interrupts:
			}
		}
		cancel()
	})

	f, ok := streams.Stdin.(*os.File)
	if !ok || !c.tty || !term.IsTerminal(int(f.Fd())) {
		return cl.Exec(ctx, id, req, streams)
	}
	fd := int(f.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, err
	}
	defer func() { _ = term.Restore(fd, old) }()
	size := func() boxexec.Size {
		w, h, _ := term.GetSize(fd)
		return boxexec.Size{Rows: uint16(h), Cols: uint16(w)}
	}
	req.Size = size()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	resize := make(chan boxexec.Size, 1)
	streams.Resize = resize
	relays.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
			}
			select { // only the newest size matters
			case <-resize:
			default:
			}
			resize <- size()
		}
	})
	return cl.Exec(ctx, id, req, streams)
}

// timed runs op on the command's environment and says how long it took.
func timed(op func(*api.Client, context.Context, string) error) func(context.Context, command, *api.Client, stdio) (int, error) {
	return func(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
		start := time.Now()
		if err := op(cl, ctx, c.args[0]); err != nil {
			return 0, err
		}
		return 0, took(std, start, c.name+" "+c.args[0])
	}
}

// took says how long what took since start, as the lab's scripts read it.
func took(std stdio, start time.Time, what string) error {
	_, err := fmt.Fprintf(std.out, "%s took %.1fms\n", what, float64(time.Since(start).Microseconds())/1000)
	return err
}
