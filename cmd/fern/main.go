// Command fern is the Fern daemon and its command-line client.
//
//	fern daemon [flags]                      run the node daemon and CSI plugin
//	fern create <env> [--repo DIR|-] [--path P] make an environment, asleep; P is its workspace
//	fern ls                                  list environments
//	fern which <path>                        print the environment whose workspace holds path
//	fern rm <env>                            delete an environment
//	fern snapshot [--replace] <env> [<snap>] take a snapshot (again, with --replace)
//	fern snapshots <env>                     list snapshots
//	fern rmsnap <env> <snapshot>…            delete snapshots
//	fern restore <env> <snapshot>            restore a snapshot; the box stays asleep
//	fern exec [-i] [-t] [-it] [--cwd DIR] [-e NAME[=VALUE]]… <env> -- cmd…
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

// defaultSocket is in a directory of its own, so pods can share it through a
// hostPath.
const defaultSocket = "/run/fern/fern.sock"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run runs one invocation and returns the process exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "daemon" {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
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
	// exec passes signals on to the command; everything else stops.
	if cmd.name != "exec" {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
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
	snaps  []string // rmsnap
	repo   string   // create: a directory to copy in, or "-" for a tar stream on stdin
	path   string   // create: the workspace path; which: the path to look up
	again  bool     // snapshot --replace
	stdin  bool     // exec -i
	tty    bool     // exec -t
	here   bool     // exec --here
	dir    string   // exec --cwd
	vars   []string // exec -e, as NAME=VALUE
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
			fs.StringVar(&c.path, "path", "", "the workspace: <pool>/worktrees/<repo>/<name>; a directory there is adopted")
		}},
		{name: "ls", run: list},
		{name: "which", min: 1, max: 1, run: which},
		{name: "rm", min: 1, max: 1, run: func(ctx context.Context, c command, cl *api.Client, _ stdio) (int, error) {
			return 0, cl.Delete(ctx, c.env)
		}},
		{name: "snapshot", min: 1, max: 2, run: snapshot, flags: func(fs *flag.FlagSet, c *command) {
			fs.BoolVar(&c.again, "replace", false, "take it again if it exists")
		}},
		{name: "snapshots", min: 1, max: 1, run: snapshots},
		{name: "rmsnap", min: 2, max: -1, run: func(ctx context.Context, c command, cl *api.Client, _ stdio) (int, error) {
			for _, snap := range c.snaps {
				if err := cl.DeleteSnapshot(ctx, c.env, snap); err != nil {
					return 0, err
				}
			}
			return 0, nil
		}},
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
	switch {
	case cmd.name == "which":
		cmd.path = pos[0]
	case len(pos) > 0:
		cmd.env = pos[0]
	}
	if cmd.name == "rmsnap" {
		cmd.snaps = pos[1:]
	} else if len(pos) > 1 {
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
	fs.BoolVar(&cmd.here, "here", false, "use the environment holding the current directory, and run in it")
	fs.StringVar(&cmd.dir, "cwd", "", "directory to run in (default: the workspace)")
	fs.Func("e", "pass `NAME` (with its value here) or NAME=VALUE on to the command", func(v string) error {
		if !strings.Contains(v, "=") {
			val, ok := os.LookupEnv(v)
			if !ok {
				return nil // an unset name passes nothing
			}
			v += "=" + val
		}
		cmd.vars = append(cmd.vars, v)
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
		cmd.env, pos = pos[0], pos[1:]
	}
	cmd.stdin, cmd.tty = cmd.stdin || it, cmd.tty || it
	cmd.argv = pos
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
	_, _ = fmt.Fprintln(tw, "ENV\tSTATE\tCREATED\tWORKSPACE") // tabwriter reports errors on Flush
	for _, e := range envs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.ID, e.State, e.Created.Format(time.RFC3339), e.Workspace)
	}
	return 0, tw.Flush()
}

func which(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	env, err := cl.FindByPath(ctx, c.path)
	if err != nil {
		return 0, err
	}
	_, err = fmt.Fprintln(std.out, env.ID)
	return 0, err
}

func snapshot(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	start := time.Now()
	take := cl.Snapshot
	if c.again {
		if c.snap == "" {
			return 0, errors.New("--replace needs a snapshot ID")
		}
		take = cl.ReplaceSnapshot
	}
	snap, err := take(ctx, c.env, c.snap)
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
	opts := api.CreateOptions{Path: c.path}
	switch c.repo {
	case "":
		env, err = cl.Create(ctx, c.env, opts)
	case "-":
		opts.Seed = std.in
		env, err = cl.Create(ctx, c.env, opts)
	default:
		env, err = createFromDir(ctx, c, opts, cl, std.err)
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
func createFromDir(ctx context.Context, c command, opts api.CreateOptions, cl *api.Client, stderr io.Writer) (api.Env, error) {
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
	opts.Seed = pr
	env, err := cl.Create(ctx, c.env, opts)
	// If the daemon stopped reading (it refused, or the environment exists
	// and needs no seed), tar must not block writing to the pipe.
	_ = pr.CloseWithError(errors.New("upload ended"))
	reader.Wait()
	// A failed tar fails the upload, so then tar's error says best what went
	// wrong. Otherwise a tar error is only the broken pipe of an upload the
	// daemon didn't read to the end.
	var apiErr *api.Error
	if err != nil && tarErr != nil && !errors.As(err, &apiErr) {
		err = tarErr
	}
	return env, err
}

func execute(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	req := boxexec.Request{Argv: c.argv, TTY: c.tty, Dir: c.dir, Env: c.vars}
	if c.here {
		wd, err := os.Getwd()
		if err != nil {
			return 0, err
		}
		env, err := cl.FindByPath(ctx, wd)
		if err != nil {
			return 0, err
		}
		c.env = env.ID
		if req.Dir == "" {
			req.Dir = wd
		}
	}
	stdin := std.in
	if !c.stdin {
		stdin = nil
	}
	var resize chan boxexec.Size
	var resizer sync.WaitGroup
	defer resizer.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The first interrupt goes to the command, which decides how to stop (T3
	// stops Claude with SIGTERM); a second one gives up on it.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	forward := make(chan os.Signal, 4)
	resizer.Go(func() {
		interrupted := false
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigs:
				if sig != syscall.SIGHUP {
					if interrupted {
						cancel()
						return
					}
					interrupted = true
				}
				select {
				case forward <- sig:
				default:
				}
			}
		}
	})
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
					// Block rather than drop: the newest size must arrive.
					// winch holds one more, so a burst collapses to it.
					select {
					case resize <- size():
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}
	return cl.Exec(ctx, c.env, req, boxexec.Streams{
		Stdin: stdin, Stdout: std.out, Stderr: std.err, Resize: resize, Signal: forward,
	})
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }
