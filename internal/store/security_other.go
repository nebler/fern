//go:build !unix

package store

import "os"

func validateOwnership(os.FileInfo) error { return nil }
