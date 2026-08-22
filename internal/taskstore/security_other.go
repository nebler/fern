//go:build !unix

package taskstore

import "os"

func validateOwnership(os.FileInfo) error { return nil }
