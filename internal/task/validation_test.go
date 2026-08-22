package task

import (
	"encoding/json"
	"errors"
	"math"
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
		{"task", "tsk_", func(v string) error { _, err := ParseTaskID(v); return err }},
		{"attempt", "att_", func(v string) error { _, err := ParseAttemptID(v); return err }},
		{"receipt", "rcp_", func(v string) error { _, err := ParseReceiptID(v); return err }},
		{"event", "fev_", func(v string) error { _, err := ParseEventID(v); return err }},
		{"approval", "apr_", func(v string) error { _, err := ParseApprovalID(v); return err }},
		{"result", "res_", func(v string) error { _, err := ParseResultID(v); return err }},
		{"verification", "ver_", func(v string) error { _, err := ParseVerificationID(v); return err }},
		{"publication", "pub_", func(v string) error { _, err := ParsePublicationID(v); return err }},
		{"operation", "op_", func(v string) error { _, err := ParsePublicationOperationID(v); return err }},
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
		if _, err := ParseInstallationID(v); err != nil {
			t.Errorf("installation %q: %v", v, err)
		}
		if _, err := ParsePullRequestNumber(v); err != nil {
			t.Errorf("PR %q: %v", v, err)
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
	for _, parse := range []func(string) error{
		func(value string) error { _, err := ParseRepositoryID(value); return err },
		func(value string) error { _, err := ParseInstallationID(value); return err },
		func(value string) error { _, err := ParsePullRequestNumber(value); return err },
	} {
		if err := parse("9223372036854775807"); err != nil {
			t.Fatalf("maximum signed SQLite integer rejected: %v", err)
		}
		if err := parse("9223372036854775808"); err == nil {
			t.Fatal("numeric identity exceeding SQLite INTEGER was accepted")
		}
	}
}

func TestRequestHash(t *testing.T) {
	v := strings.Repeat("ab", 32)
	h, err := ParseRequestHash(v)
	if err != nil || h.String() != v {
		t.Fatalf("round trip = %q, %v", h, err)
	}
	for _, invalid := range []string{"", strings.ToUpper(v), strings.Repeat("z", 64), v[:63]} {
		if _, err := ParseRequestHash(invalid); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%q accepted", invalid)
		}
	}
}

func TestCursorWire(t *testing.T) {
	for _, v := range []string{"0", "1", "1842", "9223372036854775807"} {
		c, err := ParseCursorWire(v)
		if err != nil || c.String() != v {
			t.Errorf("%q round trip: %v, %v", v, c, err)
		}
		encoded, err := json.Marshal(c)
		if err != nil || string(encoded) != `"`+v+`"` {
			t.Errorf("marshal %q = %s, %v", v, encoded, err)
		}
		var decoded Cursor
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != c {
			t.Errorf("unmarshal %q = %v, %v", encoded, decoded, err)
		}
	}
	for _, v := range []string{"", "00", "01", "+1", "-1", " 1", "1.0", "9223372036854775808"} {
		if _, err := ParseCursorWire(v); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%q accepted", v)
		}
	}
	if c, err := ParseAfterCursor(""); err != nil || c != 0 {
		t.Errorf("omitted after = %v, %v", c, err)
	}
	if err := Cursor(0).ValidateEvent(); !errors.Is(err, ErrInvalidCursor) {
		t.Error("zero event cursor accepted")
	}
	if err := Cursor(math.MaxInt64).ValidateEvent(); err != nil {
		t.Error(err)
	}
	var c Cursor
	for _, raw := range []string{`1`, `"01"`, `"-1"`, `null`} {
		if err := json.Unmarshal([]byte(raw), &c); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("JSON %s accepted", raw)
		}
	}
}
