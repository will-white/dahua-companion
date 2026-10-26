#!/bin/bash
# Renders release notes for the shipped changes between two refs, grouped by
# Conventional Commit type:
#
#   release-notes.sh <from-tag|""> <to-ref> [version]
#
# An empty <from-tag> means "everything up to <to-ref>" (the first release).
# Only commits touching the shipped paths are listed, matching the release
# agent's bump detection, so CI/docs churn never shows up in the notes. With
# a version, a footer shows how to pull the image.
set -euo pipefail

FROM=${1:-}
TO=${2:?usage: release-notes.sh <from-tag> <to-ref> [version]}
VERSION=${3:-}
REPO_URL=${REPO_URL:-https://github.com/will-white/dahua-companion}
IMAGE=${IMAGE:-ghcr.io/will-white/dahua-companion}
SHIPPED_PATHS=(main.go capture.go pkg go.mod go.sum Dockerfile .dockerignore)

# Fail loudly on a bad ref: git log runs inside a process substitution below,
# where its failure would otherwise just produce empty notes.
for ref in ${FROM:+"$FROM"} "$TO"; do
    git rev-parse --verify --quiet "$ref^{commit}" > /dev/null || { echo "release-notes: unknown ref $ref" >&2; exit 1; }
done

RANGE=("$TO")
if [ -n "$FROM" ]; then
    RANGE=("$FROM..$TO")
fi

breaking=() features=() fixes=() perf=() deps=() other=()
# %x09 is a tab: subjects can hold anything but tabs and newlines.
while IFS=$'\t' read -r sha subject; do
    [ -n "$sha" ] || continue
    body=$(git log -1 --format=%B "$sha")
    # Strip the type(scope)!: prefix for display.
    text=$(sed -E 's/^[A-Za-z]+(\([^)]*\))?!?: *//' <<< "$subject")
    item="- $text ([\`${sha:0:7}\`]($REPO_URL/commit/$sha))"
    if grep -qE '^[A-Za-z]+(\([^)]*\))?!:' <<< "$subject" || grep -qE '^BREAKING[- ]CHANGE:' <<< "$body"; then
        breaking+=("$item")
    elif grep -qE '^(chore|fix|build)\(deps\)' <<< "$subject"; then
        deps+=("$item")
    elif grep -qE '^feat(\([^)]*\))?:' <<< "$subject"; then
        features+=("$item")
    elif grep -qE '^fix(\([^)]*\))?:' <<< "$subject"; then
        fixes+=("$item")
    elif grep -qE '^perf(\([^)]*\))?:' <<< "$subject"; then
        perf+=("$item")
    else
        other+=("$item")
    fi
done < <(git log "${RANGE[@]}" --reverse --format='%H%x09%s' -- "${SHIPPED_PATHS[@]}")

section() {
    local title=$1
    shift
    [ "$#" -gt 0 ] || return 0
    printf '## %s\n\n' "$title"
    printf '%s\n' "$@"
    printf '\n'
}

section "⚠️ Breaking changes" ${breaking[@]+"${breaking[@]}"}
section "Features" ${features[@]+"${features[@]}"}
section "Fixes" ${fixes[@]+"${fixes[@]}"}
section "Performance" ${perf[@]+"${perf[@]}"}
section "Dependencies" ${deps[@]+"${deps[@]}"}
section "Other changes" ${other[@]+"${other[@]}"}

if [ -n "$VERSION" ]; then
    printf '## Image\n\n```sh\ndocker pull %s:%s\n```\n\n' "$IMAGE" "$VERSION"
    printf 'Multi-arch (linux/amd64, linux/arm64), cosign-signed, with SLSA provenance and SBOM attestations.\n'
    if [ -n "$FROM" ]; then
        printf '\n**Full diff:** %s/compare/%s...%s\n' "$REPO_URL" "$FROM" "$VERSION"
    fi
fi
