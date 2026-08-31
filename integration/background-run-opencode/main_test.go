package main

import "testing"

func TestPrivateBackgroundResidueIncludesCloneAuthorityFiles(t *testing.T) {
	for _, name := range []string{
		".clone-authority-run-0198d34d6a5075fbb1f2000000000201-g1-clone.json",
		".clone-marker-stage-abcdefghijkl",
	} {
		if !privateBackgroundResidue(name) {
			t.Fatalf("actual private clone marker name %q was not rejected", name)
		}
	}
}
