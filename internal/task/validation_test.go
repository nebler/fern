package task

import (
	"errors"
	"strings"
	"testing"
)

const validUUID = "0198d34d-6a50-75fb-b1f2-b4a14d70ec55"

func TestFernIDParsers(t *testing.T) {
	tests := []struct {
		name, prefix string
		parse        func(string) error
	}{
		{"workspace", "wsp_", func(v string) error { _, err := ParseWorkspaceID(v); return err }},
		{"task", "run_", func(v string) error { _, err := ParseRunID(v); return err }},
		{"result", "res_", func(v string) error { _, err := ParseResultID(v); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.parse(tt.prefix + validUUID); err != nil {
				t.Fatal(err)
			}
			invalid := []string{"", tt.prefix + strings.ToUpper(validUUID), tt.prefix + "0198d34d-6a50-65fb-b1f2-b4a14d70ec55", tt.prefix + "0198d34d-6a50-75fb-71f2-b4a14d70ec55", "bad_" + validUUID}
			for _, v := range invalid {
				if !errors.Is(tt.parse(v), ErrInvalidID) {
					t.Errorf("%q accepted", v)
				}
			}
		})
	}
}

func TestExternalIDParsers(t *testing.T) {
	for _, v := range []string{"1", "987654321", "9223372036854775807"} {
		if _, err := ParseRepositoryID(v); err != nil {
			t.Errorf("repository %q: %v", v, err)
		}
	}
	for _, v := range []string{"", "0", "01", "-1", "+1", "1.0", "9223372036854775808", "18446744073709551616"} {
		if _, err := ParseRepositoryID(v); !errors.Is(err, ErrInvalidID) {
			t.Errorf("repository %q accepted", v)
		}
	}
	sha := strings.Repeat("a", 40)
	if _, err := ParseGitOID(sha); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{sha[:39], strings.ToUpper(sha), strings.Repeat("z", 40)} {
		if _, err := ParseGitOID(v); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Git OID %q accepted", v)
		}
	}
	if _, err := ParseOpenCodeSessionID("ses_" + strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseOpenCodeMessageID("msg_" + strings.Repeat("b", 32)); err != nil {
		t.Fatal(err)
	}
}

func TestNumericIDsFitSQLiteInteger(t *testing.T) {
	if _, err := ParseRepositoryID("9223372036854775807"); err != nil {
		t.Fatalf("maximum signed SQLite integer rejected: %v", err)
	}
	if _, err := ParseRepositoryID("9223372036854775808"); err == nil {
		t.Fatal("numeric identity exceeding SQLite INTEGER was accepted")
	}
}

func TestRequestHashString(t *testing.T) {
	var h RequestHash
	for i := range h {
		h[i] = 0xab
	}
	if got, want := h.String(), strings.Repeat("ab", 32); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
