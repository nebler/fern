#!/usr/bin/env bash
set -Eeuo pipefail

for name in GITHUB_REPOSITORY GITHUB_REF RELEASE_COMMIT RELEASE_TAG; do
  [[ -n ${!name:-} ]] || { printf 'error: %s is required\n' "$name" >&2; exit 1; }
done

[[ "$RELEASE_TAG" =~ ^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$ ]] || {
  printf 'error: release tag is not semantic: %s\n' "$RELEASE_TAG" >&2
  exit 1
}
[[ "$RELEASE_COMMIT" =~ ^[0-9a-f]{40,64}$ ]] || { printf 'error: invalid release commit\n' >&2; exit 1; }
[[ "$GITHUB_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { printf 'error: invalid GitHub repository\n' >&2; exit 1; }
[[ "$GITHUB_REF" == "refs/tags/$RELEASE_TAG" ]] || { printf 'error: workflow ref does not match release tag\n' >&2; exit 1; }

[[ $(git rev-parse --verify HEAD) == "$RELEASE_COMMIT" ]] || { printf 'error: HEAD does not match release commit\n' >&2; exit 1; }
local_tag_object=$(git rev-parse --verify "refs/tags/$RELEASE_TAG")
[[ $(git cat-file -t "$local_tag_object") == tag ]] || { printf 'error: release tag is not annotated\n' >&2; exit 1; }
[[ $(git rev-parse --verify "refs/tags/$RELEASE_TAG^{commit}") == "$RELEASE_COMMIT" ]] || {
  printf 'error: local tag does not point to release commit\n' >&2
  exit 1
}

ref_json=$(gh api --method GET "repos/$GITHUB_REPOSITORY/git/ref/tags/$RELEASE_TAG")
remote_type=$(jq -er '.object.type' <<<"$ref_json")
remote_tag_object=$(jq -er '.object.sha' <<<"$ref_json")
[[ "$remote_type" == tag ]] || { printf 'error: GitHub tag is lightweight, not annotated\n' >&2; exit 1; }
[[ "$remote_tag_object" == "$local_tag_object" ]] || { printf 'error: local and GitHub tag objects differ\n' >&2; exit 1; }

tag_json=$(gh api --method GET "repos/$GITHUB_REPOSITORY/git/tags/$remote_tag_object")
jq -e '.verification.verified == true and .verification.reason == "valid"' <<<"$tag_json" >/dev/null || {
  printf 'error: GitHub did not cryptographically verify the annotated tag\n' >&2
  exit 1
}
[[ $(jq -er '.object.type' <<<"$tag_json") == commit ]] || { printf 'error: annotated tag does not directly target a commit\n' >&2; exit 1; }
[[ $(jq -er '.object.sha' <<<"$tag_json") == "$RELEASE_COMMIT" ]] || { printf 'error: verified tag does not target release commit\n' >&2; exit 1; }

printf 'verified annotated release tag %s at %s\n' "$RELEASE_TAG" "$RELEASE_COMMIT"
