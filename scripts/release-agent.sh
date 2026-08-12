#!/bin/bash
set -e

# Function to parse version
parse_version() {
    local version=$1
    local major minor patch
    
    # Strip 'v' prefix if present
    version="${version#v}"
    
    IFS='.' read -r major minor patch <<< "$version"
    
    # Default to 0 if empty
    echo "${major:-0} ${minor:-0} ${patch:-0}"
}

# Get latest tag, default to v0.0.0 if none exists
if ! LATEST_TAG=$(git describe --tags --abbrev=0 2>/dev/null); then
    LATEST_TAG="v0.0.0"
fi

echo "Latest tag: $LATEST_TAG"

# Commit range since the last tag (all commits if no tag exists yet)
RANGE=()
if [ "$LATEST_TAG" != "v0.0.0" ]; then
    RANGE=("$LATEST_TAG..HEAD")
fi

if [ -z "$(git log "${RANGE[@]}" --oneline)" ]; then
    echo "No changes since $LATEST_TAG"
    exit 0
fi

# Only artifact-affecting changes warrant a release: gate on the paths that
# reach the shipped image. CI, docs, and devcontainer churn stays silent.
# (Skipped before the first tag - everything counts for the first release.)
SHIPPED_PATHS=(main.go pkg go.mod go.sum Dockerfile .dockerignore)
FILTER=()
if [ "$LATEST_TAG" != "v0.0.0" ]; then
    if [ -z "$(git diff --name-only "$LATEST_TAG..HEAD" -- "${SHIPPED_PATHS[@]}")" ]; then
        echo "No shipped-artifact changes since $LATEST_TAG; skipping proposal"
        exit 0
    fi
    FILTER=(-- "${SHIPPED_PATHS[@]}")
fi

# The version and changelog describe the shipped artifact, so bump detection
# and the changes list only look at commits touching SHIPPED_PATHS - a
# CI-only feat must not inflate the bump. --oneline is for the human-readable
# list only; bump detection uses --format=%s / %B, since the abbreviated hash
# prefixing every --oneline line means a subject pattern like ^feat can never
# match there, and body footers do not appear in --oneline output at all.
COMMITS=$(git log "${RANGE[@]}" --oneline "${FILTER[@]}")
SUBJECTS=$(git log "${RANGE[@]}" --format=%s "${FILTER[@]}")
BODIES=$(git log "${RANGE[@]}" --format=%B "${FILTER[@]}")

# Determine bump type per Conventional Commits: a ! after the type/scope or a
# breaking-change footer is major, feat is minor, everything else is patch.
BUMP="patch"
if grep -qE '^[A-Za-z]+(\([^)]*\))?!:' <<< "$SUBJECTS" || grep -qE '^BREAKING[- ]CHANGE:' <<< "$BODIES"; then
    BUMP="major"
elif grep -qE '^feat(\([^)]*\))?:' <<< "$SUBJECTS"; then
    BUMP="minor"
fi

echo "Detected bump type: $BUMP"

# Accumulation threshold: feat/fix/breaking changes propose immediately, but
# chore-only shipped changes (e.g. a lone dependency bump) sit until the
# oldest is CHORE_MAX_AGE_DAYS old, so they batch up instead of proposing a
# release every week. A manual workflow dispatch sets FORCE_PROPOSAL=true and
# bypasses the wait - the escape hatch for shipping a dependency fix now.
CHORE_MAX_AGE_DAYS=21
if [ "${FORCE_PROPOSAL:-false}" != "true" ] && [ "$LATEST_TAG" != "v0.0.0" ] && [ "$BUMP" != "major" ] \
    && ! grep -qE '^(feat|fix)(\([^)]*\))?!?:' <<< "$SUBJECTS"; then
    OLDEST_TS=$(git log "${RANGE[@]}" --format=%ct -- "${SHIPPED_PATHS[@]}" | tail -1)
    AGE_DAYS=$(( ( $(date +%s) - OLDEST_TS ) / 86400 ))
    if [ "$AGE_DAYS" -lt "$CHORE_MAX_AGE_DAYS" ]; then
        echo "Only chore-level shipped changes (oldest ${AGE_DAYS}d < ${CHORE_MAX_AGE_DAYS}d); accumulating"
        exit 0
    fi
fi

# Calculate new version
read -r MAJOR MINOR PATCH <<< "$(parse_version "$LATEST_TAG")"

case $BUMP in
    major)
        MAJOR=$((MAJOR + 1))
        MINOR=0
        PATCH=0
        ;;
    minor)
        MINOR=$((MINOR + 1))
        PATCH=0
        ;;
    patch)
        PATCH=$((PATCH + 1))
        ;;
esac

NEW_TAG="v$MAJOR.$MINOR.$PATCH"
echo "Proposed version: $NEW_TAG"

# Generate Markdown Report
OUTPUT_FILE="release_proposal.md"

# Recorded so release-publish can tag exactly the commit this proposal was
# reviewed against, even if main moves before the approval label lands.
COMMIT_SHA=$(git rev-parse HEAD)

cat <<EOF > "$OUTPUT_FILE"
# Release Proposal

**Latest Tag:** $LATEST_TAG
**Proposed Tag:** $NEW_TAG
**Update Type:** $BUMP
**Commit:** $COMMIT_SHA

## Changes

\`\`\`text
$COMMITS
\`\`\`

This proposal was generated automatically by the Release Agent.

## Approval

To approve this release and trigger the deployment, add the **release-approved** label to this issue.
EOF

echo "Report generated at $OUTPUT_FILE"
