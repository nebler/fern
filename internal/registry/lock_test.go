package registry

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
)

func TestAcquireIsExclusiveAcrossProcesses(t *testing.T) {
	if os.Getenv("FERN_LOCK_HELPER") == "1" {
		lease, err := Acquire(os.Getenv("FERN_LOCK_DIR"), "demo")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = lease.Release()
		return
	}

	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=TestAcquireIsExclusiveAcrossProcesses")
	command.Env = append(os.Environ(), "FERN_LOCK_HELPER=1", "FERN_LOCK_DIR="+directory)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper output = %q", line)
	}
	if _, err := Acquire(directory, "demo"); err == nil {
		t.Fatal("Acquire succeeded while helper process held the lease")
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(directory, "demo")
	if err != nil {
		t.Fatalf("Acquire after helper exit: %v", err)
	}
	defer lease.Release()
}
