// Package gitref is the single source of truth for validating Git references,
// SHA-1 object IDs, GitHub owner/repository names, and repository-relative
// paths. These validators guard security-sensitive boundaries such as GitHub
// API routes, publication branch names, and result manifest paths, so packages
// must delegate here instead of keeping private copies whose rules can drift
// apart over time.
package gitref

import (
	"errors"
	"strings"
)

// maxRefBytes matches the 255-byte reference limit enforced by Git itself.
const maxRefBytes = 255

var (
	// ErrInvalidRef reports a reference name outside the canonical Git rules.
	ErrInvalidRef = errors.New("invalid Git reference name")
	// ErrInvalidOwnerRepo reports a value other than a canonical GitHub
	// OWNER/REPOSITORY full name.
	ErrInvalidOwnerRepo = errors.New("invalid GitHub owner/repository name")
)

// ValidateRef enforces the canonical Git reference-name rules: non-empty and at
// most 255 bytes of printable ASCII, no leading '-', no control characters,
// space, or '~^:?*[\', no '..' or '@{', no leading/trailing/embedded '//', no
// component starting with '.', and no case-insensitive '.lock' component
// suffix. It is the strictest union of the validators previously duplicated by
// githubapp, publication, and workspacegithub.
func ValidateRef(ref string) error {
	if len(ref) == 0 || len(ref) > maxRefBytes || !printableASCII(ref) || ref == "@" ||
		strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return ErrInvalidRef
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(strings.ToLower(component), ".lock") {
			return ErrInvalidRef
		}
	}
	return nil
}

// ValidSHA1 reports whether sha is exactly 40 lowercase hexadecimal characters.
func ValidSHA1(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for i := range len(sha) {
		if character := sha[i]; (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// ValidateOwnerRepo accepts only canonical 'owner/repository' full names:
// exactly one '/', total length 3-140, owner 1-39 characters from
// [A-Za-z0-9-] without leading/trailing '-', repository 1-100 characters from
// [A-Za-z0-9._-], never '.' or '..', and never carrying a case-insensitive
// '.git' suffix. It implements the strictest union of the rules previously
// duplicated by githubapp, publication, and workspacegithub.
func ValidateOwnerRepo(fullName string) error {
	if len(fullName) < 3 || len(fullName) > 140 || strings.Count(fullName, "/") != 1 || !printableASCII(fullName) {
		return ErrInvalidOwnerRepo
	}
	owner, name, _ := strings.Cut(fullName, "/")
	if len(owner) < 1 || len(owner) > 39 || owner[0] == '-' || owner[len(owner)-1] == '-' ||
		len(name) < 1 || len(name) > 100 || name == "." || name == ".." ||
		strings.HasSuffix(strings.ToLower(name), ".git") {
		return ErrInvalidOwnerRepo
	}
	for i := range len(owner) {
		if character := owner[i]; !asciiAlphanumeric(character) && character != '-' {
			return ErrInvalidOwnerRepo
		}
	}
	for i := range len(name) {
		character := name[i]
		if !asciiAlphanumeric(character) && character != '-' && character != '_' && character != '.' {
			return ErrInvalidOwnerRepo
		}
	}
	return nil
}

// ValidPath reports whether path is a safe repository-relative slash path: at
// most 4096 bytes, no NUL bytes, no leading/trailing '/', and no '.', '..',
// or empty components.
func ValidPath(path string) bool {
	if len(path) == 0 || len(path) > 4096 || path[0] == '/' || path[len(path)-1] == '/' || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

// ValidPathBytes is the []byte form of ValidPath for callers holding raw
// decoded bytes.
func ValidPathBytes(path []byte) bool {
	return ValidPath(string(path))
}

func printableASCII(value string) bool {
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func asciiAlphanumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}
