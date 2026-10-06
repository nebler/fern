// Command agekey writes a fresh age X25519 identity file and prints its
// recipient, so the release harness can exercise encrypted backups without an
// age-keygen installation.
package main

import (
	"fmt"
	"os"

	"filippo.io/age"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./integration/release/agekey IDENTITY-FILE")
		os.Exit(2)
	}
	identity, err := age.GenerateX25519Identity()
	if err == nil {
		err = os.WriteFile(os.Args[1], []byte(identity.String()+"\n"), 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(identity.Recipient())
}
