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
			fmt.Fprintf(stderr, "fern daemon: %v\n", err)
			return 1
		}
		return 0
	}
	cmd, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "fern: %v\n", err)
		return 2
	}
	code, err := cmd.run(ctx, api.NewClient(cmd.socket), stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "fern %s: %v\n", cmd.name, err)
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
	if global.NArg() == 0 {
		return command{}, errors.New("usage: fern <create|ls|rm|snapshot|snapshots|restore|exec|sleep|wake> …")
	}
	// How many positional arguments each command takes, at least and at most.
	positional := map[string][2]int{
		"create": {1, 1}, "ls": {0, 0}, "rm": {1, 1}, "snapshot": {1, 2}, "snapshots": {1, 1},
		"restore": {2, 2}, "exec": {1, -1}, "sleep": {1, 1}, "wake": {1, 1},
	}
	cmd := command{name: global.Arg(0), socket: socket}
	limits, ok := positional[cmd.name]
	if !ok {
		return command{}, fmt.Errorf("unknown command %q", cmd.name)
	}
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var it bool
	switch cmd.name {
	case "create":
		fs.StringVar(&cmd.repo, "repo", "", "directory to copy into the environment, or - for a tar stream")
	case "exec":
		fs.BoolVar(&cmd.stdin, "i", false, "send stdin")
		fs.BoolVar(&cmd.tty, "t", false, "allocate a terminal")
		fs.BoolVar(&it, "it", false, "-i and -t")
	}
	var pos []string
	var err error
	if cmd.name == "exec" {
		// Everything after the environment belongs to the command.
		err = fs.Parse(global.Args()[1:])
		pos = fs.Args()
	} else {
		pos, err = parseInterleaved(fs, global.Args()[1:])
	}
	if err != nil {
		return command{}, err
	}
	if len(pos) < limits[0] || (limits[1] >= 0 && len(pos) > limits[1]) {
		return command{}, fmt.Errorf("%s: wrong number of arguments", cmd.name)
	}
	if len(pos) > 0 {
		cmd.env = pos[0]
	}
	if len(pos) > 1 && cmd.name != "exec" {
		cmd.snap = pos[1]
	}
	if cmd.name == "exec" {
		cmd.stdin, cmd.tty = cmd.stdin || it, cmd.tty || it
		cmd.argv = pos[1:]
		if len(cmd.argv) > 0 && cmd.argv[0] == "--" {
			cmd.argv = cmd.argv[1:]
		}
		if len(cmd.argv) == 0 {
			return command{}, errors.New("exec: no command given")
		}
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

func (c command) run(ctx context.Context, cl *api.Client, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	start := time.Now()
	switch c.name {
	case "create":
		return 0, c.create(ctx, cl, stdin, stdout)
	case "ls":
		envs, err := cl.List(ctx)
		if err != nil {
			return 0, err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ENV\tSTATE\tHEAD\tREPO\tCREATED")
		for _, e := range envs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.ID, e.State, e.Head, e.Repo, e.Created.Format(time.RFC3339))
		}
		return 0, tw.Flush()
	case "rm":
		return 0, cl.Delete(ctx, c.env)
	case "snapshot":
		snap, err := cl.Snapshot(ctx, c.env, c.snap)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "snapshot %s/%s took %s\n", c.env, snap.ID, ms(time.Since(start)))
		return 0, nil
	case "snapshots":
		snaps, err := cl.Snapshots(ctx, c.env)
		if err != nil {
			return 0, err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SNAPSHOT\tCREATED")
		for _, s := range snaps {
			fmt.Fprintf(tw, "%s\t%s\n", s.ID, s.Created.Format(time.RFC3339))
		}
		return 0, tw.Flush()
	case "restore":
		if err := cl.Restore(ctx, c.env, c.snap); err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "restore %s to %s took %s\n", c.env, c.snap, ms(time.Since(start)))
		return 0, nil
	case "sleep", "wake":
		op := cl.Sleep
		if c.name == "wake" {
			op = cl.Wake
		}
		if err := op(ctx, c.env); err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "%s %s took %s\n", c.name, c.env, ms(time.Since(start)))
		return 0, nil
	case "exec":
		return c.exec(ctx, cl, stdin, stdout, stderr)
	}
	return 0, fmt.Errorf("unknown command %q", c.name)
}

func (c command) create(ctx context.Context, cl *api.Client, stdin io.Reader, stdout io.Writer) error {
	var seed io.Reader
	repo := c.repo
	switch c.repo {
	case "":
	case "-":
		seed, repo = stdin, ""
	default:
		// Stream the directory as a tar archive, the same format as "-".
		tar := exec.CommandContext(ctx, "tar", "-C", c.repo, "-c", "-f", "-", ".")
		out, err := tar.StdoutPipe()
		if err != nil {
			return err
		}
		if err := tar.Start(); err != nil {
			return fmt.Errorf("read %s: %w", c.repo, err)
		}
		defer func() { _ = tar.Wait() }()
		seed = out
	}
	env, err := cl.Create(ctx, c.env, repo, seed)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "environment %s is %s\n", env.ID, env.State)
	return nil
}

func (c command) exec(ctx context.Context, cl *api.Client, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	req := boxexec.Request{Argv: c.argv, TTY: c.tty}
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
	return cl.Exec(ctx, c.env, req, stdin, stdout, stderr, resize)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000) }
