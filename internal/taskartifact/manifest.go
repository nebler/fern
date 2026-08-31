package taskartifact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nebler/fern/internal/task"
)

type artifactManifest struct {
	Version               int
	RepositoryID          task.RepositoryID
	WorkspaceID           task.WorkspaceID
	TaskID                task.TaskID
	AttemptID             task.AttemptID
	Generation            int64
	SealRequestID         task.SealRequestID
	ImageIdentity         string
	Profile               string
	ProfileSHA256         Digest
	EnvironmentSHA256     Digest
	ResourceSpecVersion   int
	OpenCodeSessionID     task.OpenCodeSessionID
	OpenCodeMessageID     task.OpenCodeMessageID
	SnapshotPolicyVersion string
	CompletionAuthority   string
	Base                  task.GitOID
	Result                task.GitOID
	Tree                  task.GitOID
	EpochSecond           int64
	Changes               []ChangeEntry
	ChangesSHA256         Digest
	BundleSHA256          Digest
	BundleBytes           int64
}

// manifestWire is intentionally a fixed struct with no maps or omitempty
// fields. decodeManifest re-encodes it byte-for-byte to reject unknown fields,
// duplicates, alternate ordering, whitespace, and alternate JSON spellings.
type manifestWire struct {
	Version               int                    `json:"version"`
	RepositoryID          task.RepositoryID      `json:"repository_id"`
	WorkspaceID           task.WorkspaceID       `json:"workspace_id"`
	TaskID                task.TaskID            `json:"task_id"`
	AttemptID             task.AttemptID         `json:"attempt_id"`
	Generation            int64                  `json:"generation"`
	SealRequestID         task.SealRequestID     `json:"seal_request_id"`
	ImageIdentity         string                 `json:"image_identity"`
	Profile               string                 `json:"profile"`
	ProfileSHA256         string                 `json:"profile_sha256"`
	EnvironmentSHA256     string                 `json:"environment_sha256"`
	ResourceSpecVersion   int                    `json:"resource_spec_version"`
	OpenCodeSessionID     task.OpenCodeSessionID `json:"opencode_session_id"`
	OpenCodeMessageID     task.OpenCodeMessageID `json:"opencode_message_id"`
	SnapshotPolicyVersion string                 `json:"snapshot_policy_version"`
	CompletionAuthority   string                 `json:"completion_authority"`
	Base                  task.GitOID            `json:"base"`
	Result                task.GitOID            `json:"result"`
	Tree                  task.GitOID            `json:"tree"`
	EpochSecond           int64                  `json:"epoch_second"`
	Changes               []ChangeEntry          `json:"changes"`
	ChangesSHA256         string                 `json:"changes_sha256"`
	BundleSHA256          string                 `json:"bundle_sha256"`
	BundleBytes           int64                  `json:"bundle_bytes"`
}

func encodeManifest(manifest artifactManifest) ([]byte, Digest, error) {
	if err := validateManifest(manifest); err != nil {
		return nil, Digest{}, err
	}
	wire := manifestWire{
		Version: manifest.Version, RepositoryID: manifest.RepositoryID, WorkspaceID: manifest.WorkspaceID,
		TaskID: manifest.TaskID, AttemptID: manifest.AttemptID, Generation: manifest.Generation, SealRequestID: manifest.SealRequestID,
		ImageIdentity: manifest.ImageIdentity, Profile: manifest.Profile, ProfileSHA256: manifest.ProfileSHA256.String(),
		EnvironmentSHA256: manifest.EnvironmentSHA256.String(), ResourceSpecVersion: manifest.ResourceSpecVersion,
		OpenCodeSessionID: manifest.OpenCodeSessionID, OpenCodeMessageID: manifest.OpenCodeMessageID,
		SnapshotPolicyVersion: manifest.SnapshotPolicyVersion, CompletionAuthority: manifest.CompletionAuthority,
		Base: manifest.Base, Result: manifest.Result, Tree: manifest.Tree, EpochSecond: manifest.EpochSecond,
		Changes: manifest.Changes, ChangesSHA256: manifest.ChangesSHA256.String(), BundleSHA256: manifest.BundleSHA256.String(), BundleBytes: manifest.BundleBytes,
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, Digest{}, err
	}
	encoded = append(encoded, '\n')
	return encoded, sha256Bytes(encoded), nil
}

func decodeManifest(encoded []byte) (artifactManifest, Digest, error) {
	var wire manifestWire
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' || json.Unmarshal(encoded, &wire) != nil {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: malformed manifest", ErrVerification)
	}
	profileDigest, err := ParseDigest(wire.ProfileSHA256)
	if err != nil {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: profile digest", ErrVerification)
	}
	environmentDigest, err := ParseDigest(wire.EnvironmentSHA256)
	if err != nil {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: environment digest", ErrVerification)
	}
	changesDigest, err := ParseDigest(wire.ChangesSHA256)
	if err != nil {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: changes digest", ErrVerification)
	}
	bundleDigest, err := ParseDigest(wire.BundleSHA256)
	if err != nil {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: bundle digest", ErrVerification)
	}
	manifest := artifactManifest{
		Version: wire.Version, RepositoryID: wire.RepositoryID, WorkspaceID: wire.WorkspaceID, TaskID: wire.TaskID,
		AttemptID: wire.AttemptID, Generation: wire.Generation, SealRequestID: wire.SealRequestID,
		ImageIdentity: wire.ImageIdentity, Profile: wire.Profile, ProfileSHA256: profileDigest,
		EnvironmentSHA256: environmentDigest, ResourceSpecVersion: wire.ResourceSpecVersion,
		OpenCodeSessionID: wire.OpenCodeSessionID, OpenCodeMessageID: wire.OpenCodeMessageID,
		SnapshotPolicyVersion: wire.SnapshotPolicyVersion, CompletionAuthority: wire.CompletionAuthority,
		Base: wire.Base, Result: wire.Result, Tree: wire.Tree, EpochSecond: wire.EpochSecond,
		Changes: wire.Changes, ChangesSHA256: changesDigest, BundleSHA256: bundleDigest, BundleBytes: wire.BundleBytes,
	}
	canonical, manifestDigest, err := encodeManifest(manifest)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: noncanonical manifest", ErrVerification)
	}
	return manifest, manifestDigest, nil
}

func validateManifest(manifest artifactManifest) error {
	if manifest.Version != 2 || manifest.EpochSecond < 0 || manifest.EpochSecond > 253402300799 || manifest.BundleBytes <= 0 {
		return fmt.Errorf("%w: manifest metadata", ErrVerification)
	}
	if _, err := task.ParseRepositoryID(strconv.FormatUint(uint64(manifest.RepositoryID), 10)); err != nil {
		return fmt.Errorf("%w: repository ID", ErrVerification)
	}
	if _, err := task.ParseWorkspaceID(string(manifest.WorkspaceID)); err != nil {
		return fmt.Errorf("%w: workspace ID", ErrVerification)
	}
	if _, err := task.ParseTaskID(string(manifest.TaskID)); err != nil {
		return fmt.Errorf("%w: task ID", ErrVerification)
	}
	if _, err := task.ParseAttemptID(string(manifest.AttemptID)); err != nil {
		return fmt.Errorf("%w: attempt ID", ErrVerification)
	}
	if manifest.Generation <= 0 || manifest.Generation > maxGeneration {
		return fmt.Errorf("%w: generation", ErrVerification)
	}
	if _, err := task.ParseSealRequestID(string(manifest.SealRequestID)); err != nil {
		return fmt.Errorf("%w: seal request ID", ErrVerification)
	}
	if !validImageIdentity(manifest.ImageIdentity) || !validProfile(manifest.Profile) || !validDigest(manifest.ProfileSHA256) || !validDigest(manifest.EnvironmentSHA256) {
		return fmt.Errorf("%w: execution identity", ErrVerification)
	}
	if manifest.ResourceSpecVersion != ResourceSpecVersion || manifest.SnapshotPolicyVersion != SnapshotPolicyV1 || manifest.CompletionAuthority != CompletionUserSeal {
		return fmt.Errorf("%w: policy identity", ErrVerification)
	}
	if _, err := task.ParseOpenCodeSessionID(string(manifest.OpenCodeSessionID)); err != nil {
		return fmt.Errorf("%w: OpenCode session ID", ErrVerification)
	}
	if _, err := task.ParseOpenCodeMessageID(string(manifest.OpenCodeMessageID)); err != nil {
		return fmt.Errorf("%w: OpenCode message ID", ErrVerification)
	}
	for _, oid := range []task.GitOID{manifest.Base, manifest.Result, manifest.Tree} {
		if _, err := task.ParseGitOID(string(oid)); err != nil {
			return fmt.Errorf("%w: manifest object ID", ErrVerification)
		}
	}
	if (manifest.Result == manifest.Base) != (len(manifest.Changes) == 0) {
		return fmt.Errorf("%w: manifest outcome", ErrVerification)
	}
	if manifest.Changes == nil {
		return fmt.Errorf("%w: changes must be an array", ErrVerification)
	}
	var previous []byte
	for index, entry := range manifest.Changes {
		path, err := base64.StdEncoding.Strict().DecodeString(entry.PathBase64)
		if err != nil || !safeArtifactPath(path) || index > 0 && bytes.Compare(previous, path) >= 0 {
			return fmt.Errorf("%w: manifest path", ErrVerification)
		}
		previous = path
		if !validChange(entry) {
			return fmt.Errorf("%w: manifest entry", ErrVerification)
		}
	}
	_, changesDigest, err := canonicalChanges(manifest.Changes)
	if err != nil || changesDigest != manifest.ChangesSHA256 {
		return fmt.Errorf("%w: changes digest", ErrVerification)
	}
	if !validDigest(manifest.BundleSHA256) {
		return fmt.Errorf("%w: bundle digest", ErrVerification)
	}
	return nil
}

// canonicalChanges is the encoding/json representation of the non-nil changes
// array with no trailing newline or other whitespace. The empty array is [].
func canonicalChanges(changes []ChangeEntry) ([]byte, Digest, error) {
	if changes == nil {
		return nil, Digest{}, fmt.Errorf("%w: nil changes", ErrVerification)
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return nil, Digest{}, err
	}
	return encoded, sha256Bytes(encoded), nil
}

func validDigest(digest Digest) bool { return digest.value != ([32]byte{}) }

func validImageIdentity(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := ParseDigest(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validProfile(value string) bool {
	if len(value) == 0 || len(value) > maxProfileBytes {
		return false
	}
	if first := value[0]; (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') && (first < '0' || first > '9') {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && !strings.ContainsRune("-._+", rune(character)) {
			return false
		}
	}
	return true
}

func validChange(entry ChangeEntry) bool {
	validVersion := func(version *FileVersion) bool {
		if version == nil || !validMode(version.Mode) || version.Size < 0 {
			return false
		}
		_, err := task.ParseGitOID(string(version.BlobOID))
		return err == nil
	}
	switch entry.Kind {
	case "added":
		return entry.Old == nil && validVersion(entry.New)
	case "deleted":
		return validVersion(entry.Old) && entry.New == nil
	case "modified":
		return validVersion(entry.Old) && validVersion(entry.New) && *entry.Old != *entry.New
	default:
		return false
	}
}

func manifestsEqual(left, right []ChangeEntry) bool {
	leftEncoded, _, leftErr := canonicalChanges(left)
	rightEncoded, _, rightErr := canonicalChanges(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftEncoded, rightEncoded)
}
