package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeStoragePrerequisiteFailsClosed(t *testing.T) {
	t.Setenv("FERN_RUNTIME_STORAGE_ROOT", "")
	_, err := runtimeStoragePrerequisite()
	if err == nil {
		t.Fatal("missing quota prerequisite accepted")
	}
	if runtime.GOOS != "linux" && !strings.Contains(err.Error(), "Docker Desktop is unsupported") {
		t.Fatalf("missing explicit platform rejection: %v", err)
	}
}

func TestWithinHarnessRoot(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{"/quota/own", true}, {"/quota/own/state", true},
		{"/quota/own-other", false}, {"/quota", false}, {"/other", false},
	} {
		if got := withinHarnessRoot("/quota/own", test.path); got != test.want {
			t.Errorf("withinHarnessRoot(%q) = %t, want %t", test.path, got, test.want)
		}
	}
}
