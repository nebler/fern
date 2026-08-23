package task

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

type (
	WorkspaceID            string
	TaskID                 string
	AttemptID              string
	ReceiptID              string
	EventID                string
	ApprovalID             string
	SealRequestID          string
	ResultID               string
	VerificationID         string
	PublicationID          string
	PublicationOperationID string
	RepositoryID           uint64
	InstallationID         uint64
	PullRequestNumber      uint64
	GitOID                 string
	OpenCodeSessionID      string
	OpenCodeMessageID      string
)

const maxSQLiteInteger = uint64(1<<63 - 1)

func ParseWorkspaceID(v string) (WorkspaceID, error) {
	if err := validateFernID(v, "wsp_"); err != nil {
		return "", err
	}
	return WorkspaceID(v), nil
}
func ParseTaskID(v string) (TaskID, error) {
	if err := validateFernID(v, "tsk_"); err != nil {
		return "", err
	}
	return TaskID(v), nil
}
func ParseAttemptID(v string) (AttemptID, error) {
	if err := validateFernID(v, "att_"); err != nil {
		return "", err
	}
	return AttemptID(v), nil
}
func ParseReceiptID(v string) (ReceiptID, error) {
	if err := validateFernID(v, "rcp_"); err != nil {
		return "", err
	}
	return ReceiptID(v), nil
}
func ParseEventID(v string) (EventID, error) {
	if err := validateFernID(v, "fev_"); err != nil {
		return "", err
	}
	return EventID(v), nil
}
func ParseApprovalID(v string) (ApprovalID, error) {
	if err := validateFernID(v, "apr_"); err != nil {
		return "", err
	}
	return ApprovalID(v), nil
}
func ParseSealRequestID(v string) (SealRequestID, error) {
	if err := validateFernID(v, "slr_"); err != nil {
		return "", err
	}
	return SealRequestID(v), nil
}
func ParseResultID(v string) (ResultID, error) {
	if err := validateFernID(v, "res_"); err != nil {
		return "", err
	}
	return ResultID(v), nil
}
func ParseVerificationID(v string) (VerificationID, error) {
	if err := validateFernID(v, "ver_"); err != nil {
		return "", err
	}
	return VerificationID(v), nil
}
func ParsePublicationID(v string) (PublicationID, error) {
	if err := validateFernID(v, "pub_"); err != nil {
		return "", err
	}
	return PublicationID(v), nil
}
func ParsePublicationOperationID(v string) (PublicationOperationID, error) {
	if err := validateFernID(v, "op_"); err != nil {
		return "", err
	}
	return PublicationOperationID(v), nil
}

func validateFernID(v, prefix string) error {
	if len(v) > 64 || !strings.HasPrefix(v, prefix) {
		return fmt.Errorf("%w: expected %s UUIDv7", ErrInvalidID, prefix)
	}
	u := v[len(prefix):]
	if len(u) != 36 || u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
		return fmt.Errorf("%w: expected canonical UUID", ErrInvalidID)
	}
	compact := strings.ReplaceAll(u, "-", "")
	decoded, err := hex.DecodeString(compact)
	if err != nil || compact != strings.ToLower(compact) || len(decoded) != 16 {
		return fmt.Errorf("%w: expected lowercase hexadecimal UUID", ErrInvalidID)
	}
	if decoded[6]>>4 != 7 || decoded[8]>>6 != 2 {
		return fmt.Errorf("%w: expected RFC 9562 UUIDv7", ErrInvalidID)
	}
	return nil
}

func ParseRepositoryID(v string) (RepositoryID, error) {
	n, err := parsePositiveUint(v)
	if err != nil {
		return 0, fmt.Errorf("%w: repository ID", ErrInvalidID)
	}
	return RepositoryID(n), nil
}
func ParseInstallationID(v string) (InstallationID, error) {
	n, err := parsePositiveUint(v)
	if err != nil {
		return 0, fmt.Errorf("%w: installation ID", ErrInvalidID)
	}
	return InstallationID(n), nil
}
func ParsePullRequestNumber(v string) (PullRequestNumber, error) {
	n, err := parsePositiveUint(v)
	if err != nil {
		return 0, fmt.Errorf("%w: pull request number", ErrInvalidID)
	}
	return PullRequestNumber(n), nil
}

func parsePositiveUint(v string) (uint64, error) {
	if v == "" || v[0] == '0' {
		return 0, ErrInvalidID
	}
	for i := range len(v) {
		if v[i] < '0' || v[i] > '9' {
			return 0, ErrInvalidID
		}
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 || n > maxSQLiteInteger {
		return 0, ErrInvalidID
	}
	return n, nil
}

func ParseGitOID(v string) (GitOID, error) {
	if len(v) != 40 || v != strings.ToLower(v) {
		return "", fmt.Errorf("%w: Git SHA-1 must be 40 lowercase hex characters", ErrInvalidID)
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", fmt.Errorf("%w: Git SHA-1", ErrInvalidID)
	}
	return GitOID(v), nil
}

func ParseOpenCodeSessionID(v string) (OpenCodeSessionID, error) {
	if !validRandomOpenCodeID(v, "ses_") {
		return "", fmt.Errorf("%w: OpenCode session ID", ErrInvalidID)
	}
	return OpenCodeSessionID(v), nil
}

func ParseOpenCodeMessageID(v string) (OpenCodeMessageID, error) {
	if !validRandomOpenCodeID(v, "msg_") {
		return "", fmt.Errorf("%w: OpenCode message ID", ErrInvalidID)
	}
	return OpenCodeMessageID(v), nil
}

func validRandomOpenCodeID(v, prefix string) bool {
	if len(v) != len(prefix)+32 || !strings.HasPrefix(v, prefix) {
		return false
	}
	h := v[len(prefix):]
	_, err := hex.DecodeString(h)
	return err == nil && h == strings.ToLower(h)
}
