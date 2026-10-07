package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/nebler/fern/internal/atomicfile"
)

const maxAgeIdentityBytes = 64 << 10

var errUnsafeIdentityFile = errors.New("unsafe age identity file")

// repeatedFlag collects a flag given more than once, such as --recipient.
type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

// parseAgeRecipients accepts explicit age X25519 recipients.
func parseAgeRecipients(values []string) ([]age.Recipient, error) {
	result := make([]age.Recipient, 0, len(values))
	for _, value := range values {
		recipient, err := age.ParseX25519Recipient(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid age recipient: %w", err)
		}
		result = append(result, recipient)
	}
	if len(result) == 0 {
		return nil, errors.New("at least one age recipient is required")
	}
	return result, nil
}

// loadAgeIdentities reads X25519 identities from regular files that only their
// owner can access, because age identities are private keys.
func loadAgeIdentities(paths []string) ([]age.Identity, error) {
	var result []age.Identity
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("%w %s: %v", errUnsafeIdentityFile, path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%w %s: mode %04o is group or world accessible", errUnsafeIdentityFile, path, info.Mode().Perm())
		}
		payload, err := atomicfile.Read(path, maxAgeIdentityBytes)
		if err != nil {
			return nil, fmt.Errorf("%w %s: %v", errUnsafeIdentityFile, path, err)
		}
		for _, line := range strings.Split(string(payload), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			identity, err := age.ParseX25519Identity(line)
			if err != nil {
				return nil, fmt.Errorf("invalid age identity in %s", path)
			}
			result = append(result, identity)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("no age identities found")
	}
	return result, nil
}
