#!/usr/bin/env bash
set -Eeuo pipefail

[[ ${FERN_GITHUB_TEST_CONFIRM_MUTATION:-0} == 1 ]] || {
  echo 'Set FERN_GITHUB_TEST_CONFIRM_MUTATION=1 to create and delete a disposable private GitHub repository.' >&2
  exit 2
}

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
TEMP=$(mktemp -d "${TMPDIR:-/tmp}/fern-github.XXXXXX")
OWNER=$(gh api user --jq .login)
REPOSITORY="$OWNER/fern-field-test-$(date -u +%Y%m%d%H%M%S)-$RANDOM"
CREATED=0

cleanup() {
  set +e
  if [[ "$CREATED" == 1 ]]; then
    if ! gh repo delete "$REPOSITORY" --yes >/dev/null 2>&1; then
      gh repo archive "$REPOSITORY" --yes >/dev/null 2>&1
      echo "warning: archived but could not delete disposable repository $REPOSITORY" >&2
    fi
  fi
  rm -rf "$TEMP"
}
trap cleanup EXIT INT TERM

for command in docker gh git go jq; do command -v "$command" >/dev/null; done
gh auth status --hostname github.com >/dev/null
SCOPES=$(gh api -i user 2>/dev/null | awk -F': ' 'tolower($1) == "x-oauth-scopes" {print $2}')
[[ ",$SCOPES," == *", delete_repo,"* || ",$SCOPES," == *",delete_repo,"* ]] || {
  echo 'GitHub credential lacks delete_repo; run: gh auth refresh -h github.com -s delete_repo' >&2
  exit 2
}
mkdir "$TEMP/repository"
git -C "$TEMP/repository" init --initial-branch=main
git -C "$TEMP/repository" config user.name 'Fern Publication Test'
git -C "$TEMP/repository" config user.email 'fern-publication@example.invalid'
printf 'disposable Fern publication test\n' >"$TEMP/repository/README.md"
git -C "$TEMP/repository" add README.md
git -C "$TEMP/repository" commit -m 'test: initialize disposable repository'
gh repo create "$REPOSITORY" --private --source "$TEMP/repository" --remote origin --push >/dev/null
CREATED=1

cat >"$TEMP/fern.yaml" <<EOF
workspace:
  name: github-test
  image: fern/opencode:dev
  repo: $TEMP/repository
  memory: 1Gi
  env:
    OPENCODE_PASSWORD: publication-test-only
EOF
GOTOOLCHAIN=local go build -o "$TEMP/fern" "$ROOT/cmd/fern"

printf 'published change\n' >>"$TEMP/repository/README.md"
git -C "$TEMP/repository" add README.md
git -C "$TEMP/repository" commit -m 'test: publish exact change'
HEAD=$(git -C "$TEMP/repository" rev-parse HEAD)
"$TEMP/fern" github publish --config "$TEMP/fern.yaml" --operation exact --base main --title 'Fern disposable publication' >"$TEMP/publish.out"
REMOTE=$(git -C "$TEMP/repository" ls-remote origin refs/heads/fern/github-test/exact | awk '{print $1}')
[[ "$REMOTE" == "$HEAD" ]] || { echo "remote branch $REMOTE does not equal $HEAD" >&2; exit 1; }
PR_COUNT=$(gh pr list --repo "$REPOSITORY" --head fern/github-test/exact --base main --state open --json isDraft,headRefOid | jq --arg head "$HEAD" '[.[] | select(.isDraft and .headRefOid == $head)] | length')
[[ "$PR_COUNT" == 1 ]] || { echo "expected one exact draft PR, found $PR_COUNT" >&2; exit 1; }

"$TEMP/fern" github publish --config "$TEMP/fern.yaml" --operation exact --base main --title 'Fern disposable publication' >/dev/null
PR_COUNT=$(gh pr list --repo "$REPOSITORY" --head fern/github-test/exact --base main --state open --json number | jq length)
[[ "$PR_COUNT" == 1 ]] || { echo "retry created duplicate PRs" >&2; exit 1; }

BASE=$(git -C "$TEMP/repository" rev-parse HEAD~1)
git -C "$TEMP/repository" push origin "$BASE:refs/heads/fern/github-test/conflict" >/dev/null
if "$TEMP/fern" github publish --config "$TEMP/fern.yaml" --operation conflict --base main --title 'Must conflict' >"$TEMP/conflict.out" 2>&1; then
  echo 'publication overwrote a conflicting remote branch' >&2
  exit 1
fi
[[ "$(git -C "$TEMP/repository" ls-remote origin refs/heads/fern/github-test/conflict | awk '{print $1}')" == "$BASE" ]]

mkdir -p "$TEMP/repository/.github/workflows"
printf 'name: forbidden\non: push\njobs: {}\n' >"$TEMP/repository/.github/workflows/forbidden.yml"
git -C "$TEMP/repository" add .github/workflows/forbidden.yml
git -C "$TEMP/repository" commit -m 'test: forbidden workflow change'
if "$TEMP/fern" github publish --config "$TEMP/fern.yaml" --operation workflow --base main --title 'Must reject workflow' >"$TEMP/workflow.out" 2>&1; then
  echo 'publication accepted a workflow change' >&2
  exit 1
fi
test -z "$(git -C "$TEMP/repository" ls-remote origin refs/heads/fern/github-test/workflow)"

if docker inspect github-test >/dev/null 2>&1; then
  echo 'GitHub publication unexpectedly created compute' >&2
  exit 1
fi
printf 'Disposable GitHub publication rehearsal passed for %s (repository will be deleted)\n' "$REPOSITORY"
