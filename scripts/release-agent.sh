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

# --oneline is for the human-readable changes list only. Bump detection uses
# --format=%s / %B: the abbreviated hash prefixing every --oneline line means
# a subject pattern like ^feat can never match there, and body footers do not
# appear in --oneline output at all.
COMMITS=$(git log "${RANGE[@]}" --oneline)
SUBJECTS=$(git log "${RANGE[@]}" --format=%s)
BODIES=$(git log "${RANGE[@]}" --format=%B)

if [ -z "$COMMITS" ]; then
    echo "No changes since $LATEST_TAG"
    exit 0
fi

# Determine bump type per Conventional Commits: a ! after the type/scope or a
# breaking-change footer is major, feat is minor, everything else is patch.
BUMP="patch"
if grep -qE '^[A-Za-z]+(\([^)]*\))?!:' <<< "$SUBJECTS" || grep -qE '^BREAKING[- ]CHANGE:' <<< "$BODIES"; then
    BUMP="major"
elif grep -qE '^feat(\([^)]*\))?:' <<< "$SUBJECTS"; then
    BUMP="minor"
fi

echo "Detected bump type: $BUMP"

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
