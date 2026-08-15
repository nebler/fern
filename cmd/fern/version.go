package main

import (
	"errors"
	"fmt"
	"io"
)

var (
	version = "dev"
	commit  = "unknown"
)

func runVersion(args []string, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("version does not accept arguments")
	}
	_, err := fmt.Fprintf(output, "fern %s (commit %s)\n", version, commit)
	return err
}
