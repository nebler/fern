package taskpublication

import (
	"context"
	"crypto/sha256"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/task"
)

const askpassProgram = `#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' 'x-access-token' ;;
  *Password*)
    IFS= read -r credential < "${0%/*}/credential" || exit 1
    rm -f -- "${0%/*}/credential"
    printf '%s\n' "$credential"
    credential=
    ;;
  *) exit 1 ;;
esac
`

type digestWriter struct {
	hash  hash.Hash
	bytes int64
	limit int64
}

func newDigestWriter(limit int64) *digestWriter {
	return &digestWriter{hash: sha256.New(), limit: limit}
}

func (writer *digestWriter) Write(value []byte) (int, error) {
	written := len(value)
	_, _ = writer.hash.Write(value)
	writer.bytes += int64(written)
	return written, nil
}

func (writer *digestWriter) evidence() OutputEvidence {
	var sum [sha256.Size]byte
	copy(sum[:], writer.hash.Sum(nil))
	return OutputEvidence{Bytes: writer.bytes, HashedBytes: writer.bytes, SHA256: sum, Truncated: writer.bytes > writer.limit}
}

func (publisher *Publisher) proveLocalCommit(ctx context.Context, commit task.GitOID) error {
	result := publisher.runGit(ctx, nil, "--no-pager", "--no-replace-objects", "-C", publisher.repositoryPath, "cat-file", "-e", string(commit)+"^{commit}")
	if result.err != nil {
		if result.timedOut {
			return ErrGitTimeout
		}
		return ErrGitFailed
	}
	return nil
}

func (publisher *Publisher) push(ctx context.Context, identity githubapp.RepositoryIdentity, publication task.PublicationTuple) (GitEvidence, error) {
	evidence := GitEvidence{Attempted: true, ExitCode: -1}
	token, err := publisher.tokens.InstallationToken(ctx, identity)
	if err != nil {
		return evidence, sanitizedContextError(ctx, err)
	}
	if token.Identity() != identity || token.Permissions().Contents() != "write" || token.Permissions().PullRequests() != "write" {
		return evidence, githubapp.ErrInvalidInstallationToken
	}
	credential, err := token.Value(publisher.now().UTC())
	if err != nil {
		return evidence, err
	}

	directory, err := os.MkdirTemp(publisher.tempRoot, "fern-push-")
	if err != nil {
		return evidence, ErrGitFailed
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		return evidence, ErrGitFailed
	}
	helper := filepath.Join(directory, "askpass")
	credentialPath := filepath.Join(directory, "credential")
	hooks := filepath.Join(directory, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil || writeExclusive(helper, []byte(askpassProgram), 0o700) != nil || writeExclusive(credentialPath, []byte(credential+"\n"), 0o600) != nil {
		return evidence, ErrGitFailed
	}
	if err := os.Chmod(helper, 0o700); err != nil || os.Chmod(credentialPath, 0o600) != nil {
		return evidence, ErrGitFailed
	}
	credential = ""

	lease := "--force-with-lease=refs/heads/" + publication.Branch + ":" + string(publication.ExpectedRemoteOldSHA)
	remote := "https://github.com/" + publication.RepositoryFullName + ".git"
	refspec := string(publication.ResultCommit) + ":refs/heads/" + publication.Branch
	environment := []string{
		"GIT_ASKPASS=" + helper,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"HOME=" + directory,
		"LANG=C",
		"LC_ALL=C",
		"PATH=/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + directory,
	}
	arguments := []string{
		"--no-pager", "--no-replace-objects",
		"-c", "core.askPass=" + helper,
		"-c", "core.hooksPath=" + hooks,
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-C", publisher.repositoryPath,
		"push", "--porcelain", "--no-verify", lease, remote, refspec,
	}
	result := publisher.runGit(ctx, environment, arguments...)
	evidence.ExitCode = result.exitCode
	evidence.TimedOut = result.timedOut
	evidence.Stdout = result.stdout
	evidence.Stderr = result.stderr
	if result.err != nil {
		if result.timedOut {
			return evidence, ErrGitTimeout
		}
		return evidence, ErrPushFailed
	}
	return evidence, nil
}

func writeExclusive(path string, value []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(value)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

type gitResult struct {
	err      error
	exitCode int
	timedOut bool
	stdout   OutputEvidence
	stderr   OutputEvidence
}

func (publisher *Publisher) runGit(ctx context.Context, environment []string, arguments ...string) gitResult {
	commandContext, cancel := context.WithTimeout(ctx, publisher.timeout)
	defer cancel()
	stdout := newDigestWriter(publisher.outputLimit)
	stderr := newDigestWriter(publisher.outputLimit)
	command := exec.CommandContext(commandContext, publisher.gitExecutable, arguments...)
	command.Dir = publisher.repositoryPath
	if environment == nil {
		environment = []string{
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"HOME=" + publisher.tempRoot, "LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=" + publisher.tempRoot,
		}
	}
	command.Env = append([]string(nil), environment...)
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcessGroup(command)
	err := command.Run()
	result := gitResult{err: err, exitCode: -1, timedOut: commandContext.Err() != nil, stdout: stdout.evidence(), stderr: stderr.evidence()}
	if command.ProcessState != nil {
		result.exitCode = command.ProcessState.ExitCode()
	}
	return result
}

func sanitizedContextError(ctx context.Context, _ error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return githubapp.ErrRequestFailed
}

func (evidence OutputEvidence) String() string {
	return "output evidence (" + strconv.FormatInt(evidence.Bytes, 10) + " bytes)"
}

func (evidence OutputEvidence) GoString() string { return evidence.String() }

func (evidence GitEvidence) String() string   { return "sanitized Git push evidence" }
func (evidence GitEvidence) GoString() string { return evidence.String() }
