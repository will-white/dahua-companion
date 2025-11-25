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

# Get commits since last tag
# If v0.0.0, get all commits
if [ "$LATEST_TAG" = "v0.0.0" ]; then
    COMMITS=$(git log --oneline)
else
    COMMITS=$(git log "$LATEST_TAG"..HEAD --oneline)
fi

if [ -z "$COMMITS" ]; then
    echo "No changes since $LATEST_TAG"
    exit 0
fi

# Determine bump type
BUMP="patch"
if echo "$COMMITS" | grep -q "BREAKING CHANGE"; then
    BUMP="major"
elif echo "$COMMITS" | grep -q "^feat"; then
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

cat <<EOF > "$OUTPUT_FILE"
# Release Proposal

**Latest Tag:** $LATEST_TAG
**Proposed Tag:** $NEW_TAG
**Update Type:** $BUMP

## Changes

\`\`\`text
$COMMITS
\`\`\`

This proposal was generated automatically by the Release Agent.
EOF

echo "Report generated at $OUTPUT_FILE"
