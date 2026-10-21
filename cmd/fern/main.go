// Command fern is the Fern daemon and its command-line client.
//
//	fern daemon [flags]                      run the node daemon and CSI plugin
//	fern create <env> [--repo DIR|-]         make an environment, asleep
//	fern ls                                  list environments
//	fern rm <env>                            delete an environment
//	fern snapshot <env> [<snapshot>]         take a snapshot
//	fern snapshots <env>                     list snapshots
//	fern restore <env> <snapshot>            restore a snapshot; the box stays asleep
//	fern exec [-i] [-t] [-it] <env> -- cmd…  run a command, waking the box if needed
//	fern sleep <env> | fern wake <env>       stop or start the box
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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

const defaultSocket = "/run/fern.sock"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run runs one invocation and returns the process exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "daemon" {
		if err := runDaemon(ctx, args[1:], stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "fern daemon: %v\n", err)
			return 1
		}
		return 0
	}
	cmd, err := parse(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fern: %v\n", err)
		return 2
	}
	code, err := lookup(cmd.name).run(ctx, cmd, api.NewClient(cmd.socket), stdio{stdin, stdout, stderr})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fern %s: %v\n", cmd.name, err)
		return 1
	}
	return code
}

// command is a parsed CLI invocation.
type command struct {
	name   string
	socket string
	env    string
	snap   string   // snapshot, restore
	repo   string   // create: a directory to copy in, or "-" for a tar stream on stdin
	stdin  bool     // exec -i
	tty    bool     // exec -t
	argv   []string // exec
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
func subcommands() []subcommand {
	return []subcommand{
		{name: "create", min: 1, max: 1, run: create, flags: func(fs *flag.FlagSet, c *command) {
			fs.StringVar(&c.repo, "repo", "", "directory to copy into the environment, or - for a tar stream")
		}},
		{name: "ls", run: list},
		{name: "rm", min: 1, max: 1, run: func(ctx context.Context, c command, cl *api.Client, _ stdio) (int, error) {
			return 0, cl.Delete(ctx, c.env)
		}},
		{name: "snapshot", min: 1, max: 2, run: snapshot},
		{name: "snapshots", min: 1, max: 1, run: snapshots},
		{name: "restore", min: 2, max: 2, run: restore},
		{name: "exec", min: 1, max: -1, run: execute},
		{name: "sleep", min: 1, max: 1, run: timed(func(ctx context.Context, cl *api.Client, env string) error {
			return cl.Sleep(ctx, env)
		})},
		{name: "wake", min: 1, max: 1, run: timed(func(ctx context.Context, cl *api.Client, env string) error {
			return cl.Wake(ctx, env)
		})},
	}
}

// lookup returns the subcommand called name; parse has checked it exists.
func lookup(name string) subcommand {
	i := slices.IndexFunc(subcommands(), func(sc subcommand) bool { return sc.name == name })
	return subcommands()[i]
}

func parse(args []string) (command, error) {
	socket := os.Getenv("FERN_SOCKET")
	if socket == "" {
		socket = defaultSocket
	}
	global := flag.NewFlagSet("fern", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	global.StringVar(&socket, "socket", socket, "daemon socket")
	if err := global.Parse(args); err != nil {
		return command{}, err
	}
	var names []string
	for _, sc := range subcommands() {
		names = append(names, sc.name)
	}
	if global.NArg() == 0 {
		return command{}, fmt.Errorf("usage: fern <%s> …", strings.Join(names, "|"))
	}
	cmd := command{name: global.Arg(0), socket: socket}
	if !slices.Contains(names, cmd.name) {
		return command{}, fmt.Errorf("unknown command %q", cmd.name)
	}
	sc := lookup(cmd.name)
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if sc.flags != nil {
		sc.flags(fs, &cmd)
	}
	if cmd.name == "exec" {
		return parseExec(fs, cmd, global.Args()[1:])
	}
	pos, err := parseInterleaved(fs, global.Args()[1:])
	if err != nil {
		return command{}, err
	}
	if len(pos) < sc.min || (sc.max >= 0 && len(pos) > sc.max) {
		return command{}, fmt.Errorf("%s: wrong number of arguments", cmd.name)
	}
	if len(pos) > 0 {
		cmd.env = pos[0]
	}
	if len(pos) > 1 {
		cmd.snap = pos[1]
	}
	return cmd, nil
}

// parseExec parses exec's flags, which come before the environment:
// everything after it belongs to the command.
func parseExec(fs *flag.FlagSet, cmd command, args []string) (command, error) {
	var it bool
	fs.BoolVar(&cmd.stdin, "i", false, "send stdin")
	fs.BoolVar(&cmd.tty, "t", false, "allocate a terminal")
	fs.BoolVar(&it, "it", false, "-i and -t")
	if err := fs.Parse(args); err != nil {
		return command{}, err
	}
	pos := fs.Args()
	if len(pos) == 0 {
		return command{}, errors.New("exec: wrong number of arguments")
	}
	cmd.stdin, cmd.tty = cmd.stdin || it, cmd.tty || it
	cmd.env, cmd.argv = pos[0], pos[1:]
	if len(cmd.argv) > 0 && cmd.argv[0] == "--" {
		cmd.argv = cmd.argv[1:]
	}
	if len(cmd.argv) == 0 {
		return command{}, errors.New("exec: no command given")
	}
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

func list(ctx context.Context, _ command, cl *api.Client, std stdio) (int, error) {
	envs, err := cl.List(ctx)
	if err != nil {
		return 0, err
	}
	tw := tabwriter.NewWriter(std.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ENV\tSTATE\tHEAD\tREPO\tCREATED") // tabwriter reports errors on Flush
	for _, e := range envs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.ID, e.State, e.Head, e.Repo, e.Created.Format(time.RFC3339))
	}
	return 0, tw.Flush()
}

func snapshot(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	start := time.Now()
	snap, err := cl.Snapshot(ctx, c.env, c.snap)
	if err != nil {
		return 0, err
	}
	_, err = fmt.Fprintf(std.out, "snapshot %s/%s took %s\n", c.env, snap.ID, ms(time.Since(start)))
	return 0, err
}

func snapshots(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	snaps, err := cl.Snapshots(ctx, c.env)
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

func restore(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	start := time.Now()
	if err := cl.Restore(ctx, c.env, c.snap); err != nil {
		return 0, err
	}
	_, err := fmt.Fprintf(std.out, "restore %s to %s took %s\n", c.env, c.snap, ms(time.Since(start)))
	return 0, err
}

// timed runs op on the command's environment and says how long it took.
func timed(op func(ctx context.Context, cl *api.Client, env string) error) func(context.Context, command, *api.Client, stdio) (int, error) {
	return func(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
		start := time.Now()
		if err := op(ctx, cl, c.env); err != nil {
			return 0, err
		}
		_, err := fmt.Fprintf(std.out, "%s %s took %s\n", c.name, c.env, ms(time.Since(start)))
		return 0, err
	}
}

func create(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	var env api.Env
	var err error
	switch c.repo {
	case "":
		env, err = cl.Create(ctx, c.env, "", nil)
	case "-":
		env, err = cl.Create(ctx, c.env, "", std.in)
	default:
		env, err = createFromDir(ctx, c, cl, std.err)
	}
	if err != nil {
		return 0, err
	}
	_, err = fmt.Fprintf(std.out, "environment %s is %s\n", env.ID, env.State)
	return 0, err
}

// createFromDir creates the environment from the directory c.repo, streamed
// as a tar archive, the same format as "-". If tar fails, the upload fails
// with it, so the environment isn't created with files missing.
func createFromDir(ctx context.Context, c command, cl *api.Client, stderr io.Writer) (api.Env, error) {
	if info, err := os.Stat(c.repo); err != nil || !info.IsDir() {
		return api.Env{}, fmt.Errorf("--repo %s: not a directory", c.repo)
	}
	pr, pw := io.Pipe()
	tar := exec.CommandContext(ctx, "tar", "-C", c.repo, "-c", "-f", "-", ".")
	tar.Stdout, tar.Stderr = pw, stderr
	if err := tar.Start(); err != nil {
		return api.Env{}, fmt.Errorf("read %s: %w", c.repo, err)
	}
	var tarErr error
	var reader sync.WaitGroup
	reader.Go(func() {
		if err := tar.Wait(); err != nil {
			tarErr = fmt.Errorf("read %s: %w", c.repo, err)
		}
		pw.CloseWithError(tarErr) // a nil error is a clean EOF
	})
	env, err := cl.Create(ctx, c.env, c.repo, pr)
	// If the upload stopped early, tar must not block writing to the pipe.
	_ = pr.CloseWithError(errors.New("upload ended"))
	reader.Wait()
	// A failed tar fails the upload, so err already says why; tar's own
	// error after a refused upload would only be a broken pipe.
	if err == nil {
		err = tarErr
	}
	return env, err
}

func execute(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	req := boxexec.Request{Argv: c.argv, TTY: c.tty}
	stdin := std.in
	if !c.stdin {
		stdin = nil
	}
	var resize chan boxexec.Size
	var resizer sync.WaitGroup
	defer resizer.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if f, ok := stdin.(*os.File); ok && c.tty && term.IsTerminal(int(f.Fd())) {
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
		resize = make(chan boxexec.Size, 1)
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		resizer.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-winch:
					select {
					case resize <- size():
					default:
					}
				}
			}
		})
	}
	return cl.Exec(ctx, c.env, req, stdin, std.out, std.err, resize)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }
