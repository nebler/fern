#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-release-workflow.XXXXXX")
trap 'rm -rf "$TEMP"' EXIT

python3 - "$ROOT/.github/workflows/release.yml" "$ROOT/.github/workflows/ci.yml" "$ROOT/Makefile" <<'PY'
import pathlib
import re
import sys

workflow = pathlib.Path(sys.argv[1]).read_text()
ci_workflow = pathlib.Path(sys.argv[2]).read_text()
makefile = pathlib.Path(sys.argv[3]).read_text()
uses = re.findall(r"^\s*-?\s*uses:\s*([^\s#]+)", workflow, re.MULTILINE)
assert uses, "release workflow has no actions"
for action in uses:
    assert re.fullmatch(r"[^@\s]+@[0-9a-f]{40}", action), f"action is not commit pinned: {action}"

lines = workflow.splitlines()
for index, line in enumerate(lines):
    match = re.match(r"^(\s*)run:\s*\|\s*$", line)
    if not match:
        continue
    indentation = len(match.group(1))
    body = []
    for candidate in lines[index + 1:]:
        if candidate.strip() and len(candidate) - len(candidate.lstrip()) <= indentation:
            break
        body.append(candidate)
    assert "${{" not in "\n".join(body), "GitHub expression interpolated directly into a shell script"

required = [
    "./scripts/verify-release-tag.sh",
    "go test -race ./...",
    "go vet ./...",
    "./scripts/test-critical-coverage.sh",
    "./scripts/test-deployment.sh",
    "./integration/release/run.sh",
    "./integration/upgrade/run.sh",
    "./integration/background-run-qualification/run.sh",
    "bun install --frozen-lockfile",
    "bun run smoke",
    "./test/docker-cli-smoke.sh fern/opencode-background:plugin-release",
    "docker/build-push-action@",
    "cosign sign",
    "cosign attest",
    "cosign verify-attestation",
    "gh attestation verify",
    "push-to-registry: true",
    'shasum -a 256 -c SHA256SUMS',
]
for value in required:
    assert value in workflow, f"release workflow is missing required control: {value}"

validate, publish = workflow.split("\n  publish:\n", 1)
qualification = "./integration/background-run-qualification/run.sh"
assert qualification in validate, "Background Run qualification is not a release validation gate"
assert re.search(r"^\s+context: images/opencode-background-source$", publish, re.MULTILINE), \
    "release does not publish the qualified Background Run source image"
assert not re.search(r"^\s+context: images/opencode$", publish, re.MULTILINE), \
    "release still publishes the retired persistent workspace image"
assert "platforms: linux/amd64" in publish and "platforms: linux/arm64" in publish, \
    "release does not build both source-image architectures independently"
assert "candidate-${{ github.sha }}-amd64" in publish and "candidate-${{ github.sha }}-arm64" in publish, \
    "release source images are not isolated as candidate artifacts"
assert "FERN_OPENCODE_BACKGROUND_SOURCE_BUILD=0 ./integration/background-run-qualification/run.sh" in publish, \
    "release does not qualify the exact pushed candidate digests"
assert "docker buildx imagetools create" in publish, "release does not promote qualified candidate digests"
assert publish.index("Qualify exact candidate images") < publish.index("Promote only qualified candidate digests"), \
    "release promotes source images before qualification"
assert "org.opencontainers.image.source=https://github.com/${{ github.repository }}" not in publish, \
    "release overwrites the qualified upstream source identity"

assert qualification in ci_workflow, "CI is missing authoritative Background Run qualification"
assert "oven-sh/setup-bun@0c5077e51419868618aeaa5fe8019c62421857d6" in workflow, \
    "release validation is missing commit-pinned Bun setup"
assert "oven-sh/setup-bun@0c5077e51419868618aeaa5fe8019c62421857d6" in ci_workflow, \
    "CI is missing commit-pinned Bun setup"
assert "bun-version: 1.3.14" in workflow and "bun-version: 1.3.14" in ci_workflow, \
    "plugin qualification must pin Bun 1.3.14"
assert "./test/docker-cli-smoke.sh fern/opencode-background:plugin-ci" in ci_workflow, \
    "CI is missing packed plugin compatibility against OpenCode 1.18.16"
qualification_job = re.search(
    r"^  background-qualification:\n(?P<body>.*?)(?=^  [a-zA-Z0-9_-]+:|\Z)",
    ci_workflow,
    re.MULTILINE | re.DOTALL,
)
assert qualification_job, "CI is missing a dedicated Background Run qualification job"
body = qualification_job.group("body")
assert "runs-on: ubuntu-latest" in body, "Background Run qualification must run on Linux"
timeout = re.search(r"timeout-minutes:\s*([0-9]+)", body)
assert timeout and 1 <= int(timeout.group(1)) <= 45, "Background Run qualification must have a bounded timeout"
assert re.search(r"^test-background-qualification:\n\t\./integration/background-run-qualification/run\.sh$", makefile, re.MULTILINE), \
    "Makefile is missing the authoritative Background Run qualification target"
PY

FIXTURE="$TEMP/repository"
mkdir -p "$FIXTURE" "$TEMP/bin"
git -C "$FIXTURE" init -q
git -C "$FIXTURE" config user.name 'Fern release workflow test'
git -C "$FIXTURE" config user.email 'release-workflow-test@fern.invalid'
printf 'fixture\n' >"$FIXTURE/file"
git -C "$FIXTURE" add file
git -C "$FIXTURE" commit -qm fixture
git -C "$FIXTURE" tag -a v1.2.3 -m v1.2.3
COMMIT=$(git -C "$FIXTURE" rev-parse HEAD)
TAG_OBJECT=$(git -C "$FIXTURE" rev-parse refs/tags/v1.2.3)

cat >"$TEMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -eu
endpoint=${*: -1}
case "$endpoint" in
  */git/ref/tags/*)
    if [[ ${MOCK_TAG_MODE:-valid} == lightweight ]]; then
      printf '{"object":{"type":"commit","sha":"%s"}}\n' "$RELEASE_COMMIT"
    else
      printf '{"object":{"type":"tag","sha":"%s"}}\n' "$TAG_OBJECT"
    fi
    ;;
  */git/tags/*)
    verified=true
    reason=valid
    target=$RELEASE_COMMIT
    [[ ${MOCK_TAG_MODE:-valid} != unverified ]] || { verified=false; reason=unsigned; }
    [[ ${MOCK_TAG_MODE:-valid} != wrong-target ]] || target=$(printf '0%.0s' {1..40})
    printf '{"verification":{"verified":%s,"reason":"%s"},"object":{"type":"commit","sha":"%s"}}\n' "$verified" "$reason" "$target"
    ;;
  *) exit 2 ;;
esac
EOF
chmod 0755 "$TEMP/bin/gh"

verify() {
  (
    cd "$FIXTURE"
    PATH="$TEMP/bin:$PATH" TAG_OBJECT="$TAG_OBJECT" \
      GITHUB_REPOSITORY=example/fern GITHUB_REF=refs/tags/v1.2.3 \
      RELEASE_TAG=v1.2.3 RELEASE_COMMIT="$COMMIT" MOCK_TAG_MODE=${1:-valid} \
      "$ROOT/scripts/verify-release-tag.sh"
  )
}

verify valid >/dev/null
for mode in lightweight unverified wrong-target; do
  if verify "$mode" >"$TEMP/$mode.log" 2>&1; then
    printf 'error: tag verification accepted %s fixture\n' "$mode" >&2
    exit 1
  fi
done

printf 'Release workflow static and verified-tag checks passed\n'
