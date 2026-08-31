package taskartifact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

type artifactManifest struct {
	Version      int           `json:"version"`
	Base         task.GitOID   `json:"base"`
	Result       task.GitOID   `json:"result"`
	Tree         task.GitOID   `json:"tree"`
	EpochSecond  int64         `json:"epoch_second"`
	Changes      []ChangeEntry `json:"changes"`
	BundleSHA256 Digest        `json:"-"`
	BundleBytes  int64         `json:"bundle_bytes"`
}

type manifestWire struct {
	Version      int           `json:"version"`
	Base         task.GitOID   `json:"base"`
	Result       task.GitOID   `json:"result"`
	Tree         task.GitOID   `json:"tree"`
	EpochSecond  int64         `json:"epoch_second"`
	Changes      []ChangeEntry `json:"changes"`
	BundleSHA256 string        `json:"bundle_sha256"`
	BundleBytes  int64         `json:"bundle_bytes"`
}

func encodeManifest(manifest artifactManifest) ([]byte, Digest, error) {
	if err := validateManifest(manifest); err != nil {
		return nil, Digest{}, err
	}
	wire := manifestWire{manifest.Version, manifest.Base, manifest.Result, manifest.Tree, manifest.EpochSecond,
		manifest.Changes, manifest.BundleSHA256.String(), manifest.BundleBytes}
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
	digest, err := ParseDigest(wire.BundleSHA256)
	if err != nil {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: bundle digest", ErrVerification)
	}
	manifest := artifactManifest{Version: wire.Version, Base: wire.Base, Result: wire.Result, Tree: wire.Tree, EpochSecond: wire.EpochSecond,
		Changes: wire.Changes, BundleSHA256: digest, BundleBytes: wire.BundleBytes}
	canonical, manifestDigest, err := encodeManifest(manifest)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return artifactManifest{}, Digest{}, fmt.Errorf("%w: noncanonical manifest", ErrVerification)
	}
	return manifest, manifestDigest, nil
}

func validateManifest(manifest artifactManifest) error {
	if manifest.Version != 1 || manifest.EpochSecond < 0 || manifest.EpochSecond > 253402300799 || manifest.BundleBytes <= 0 {
		return fmt.Errorf("%w: manifest metadata", ErrVerification)
	}
	for _, oid := range []task.GitOID{manifest.Base, manifest.Result, manifest.Tree} {
		if _, err := task.ParseGitOID(string(oid)); err != nil {
			return fmt.Errorf("%w: manifest object ID", ErrVerification)
		}
	}
	if (manifest.Result == manifest.Base) != (len(manifest.Changes) == 0) {
		return fmt.Errorf("%w: manifest outcome", ErrVerification)
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
	return nil
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
	if len(left) != len(right) {
		return false
	}
	leftEncoded, _ := json.Marshal(left)
	rightEncoded, _ := json.Marshal(right)
	return bytes.Equal(leftEncoded, rightEncoded)
}
