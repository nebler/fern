//go:build spike

// Command probe runs one command in a box over boxexec, the way the daemon
// does, and prints how long the dial and the run took. Spike only: built
// with -tags spike, so the module's build, tests and lint skip it.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nebler/fern/internal/boxexec"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: probe ws://host:7070 token argv...")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := boxexec.Dial(ctx, os.Args[1]+boxexec.Path, os.Args[2], nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	dialed := time.Since(start)
	var out, errb bytes.Buffer
	code, err := boxexec.Run(ctx, conn, boxexec.Request{Argv: os.Args[3:]}, boxexec.Streams{Stdout: &out, Stderr: &errb})
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "dial_ms=%.2f total_ms=%.2f code=%d\n",
		float64(dialed.Microseconds())/1000, float64(time.Since(start).Microseconds())/1000, code)
	os.Stdout.Write(out.Bytes())
	os.Stderr.Write(errb.Bytes())
	os.Exit(code)
}
