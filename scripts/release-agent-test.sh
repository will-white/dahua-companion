#!/bin/bash
# Exercises every gate branch of release-agent.sh in scratch git repos: the
# no-change exit, the shipped-path gate, the chore accumulation threshold and
# its dispatch/first-release bypasses, and shipped-path-scoped bump detection.
set -e
SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-agent.sh"
NOTES_SCRIPT="$(dirname "$SCRIPT")/release-notes.sh"

newrepo() {
    dir=$(mktemp -d)
    cd "$dir"
    git init -q
    git config user.email t@t.t
    git config user.name t
    mkdir -p pkg
    echo base > main.go
    git add -A && git commit -qm "chore: init"
}

# run <name> <expected-substring> <want-proposal yes|no> [force]
run() {
    local name=$1 expect=$2 want=$3 force=${4:-false}
    rm -f release_proposal.md
    local out
    out=$(FORCE_PROPOSAL=$force bash "$SCRIPT" 2>&1)
    if ! grep -q "$expect" <<< "$out"; then
        echo "FAIL: $name"; echo "  wanted: $expect"; echo "  got: $out"; exit 1
    fi
    if [ "$want" = yes ] && [ ! -f release_proposal.md ]; then
        echo "FAIL: $name - expected a proposal file"; exit 1
    fi
    if [ "$want" = no ] && [ -f release_proposal.md ]; then
        echo "FAIL: $name - expected NO proposal file"; exit 1
    fi
    echo "PASS: $name"
}

# Commit dates far enough in the past exercise the accumulation threshold.
aged() { date -d '22 days ago' --iso-8601=seconds; }

newrepo; git tag v1.0.0
run "no commits since tag" "No changes since v1.0.0" no

newrepo; git tag v1.0.0
mkdir -p .github/workflows && echo x > .github/workflows/ci.yml
git add -A && git commit -qm "fix(docker): ci-only change"
run "ci-only changes are not shipped" "No shipped-artifact changes since v1.0.0" no

newrepo; git tag v1.0.0
echo mod > go.mod && git add -A && git commit -qm "chore(deps): bump toolchain"
run "fresh chore accumulates" "accumulating" no

newrepo; git tag v1.0.0
echo mod > go.mod && git add -A
GIT_COMMITTER_DATE=$(aged) git commit -qm "chore(deps): bump toolchain"
run "aged chore proposes" "Proposed version: v1.0.1" yes

newrepo; git tag v1.0.0
echo code > pkg/a.go && git add -A && git commit -qm "fix(dahua): stop dropping presses"
run "fix proposes immediately" "Proposed version: v1.0.1" yes

newrepo; git tag v1.0.0
echo mod > go.mod && git add -A && git commit -qm "chore(deps): bump toolchain"
run "dispatch bypasses accumulation" "Proposed version: v1.0.1" yes true

newrepo; git tag v1.0.0
echo code > pkg/a.go && git add -A && git commit -qm "feat(mqtt): new thing"
run "feat proposes minor" "Proposed version: v1.1.0" yes

newrepo; git tag v1.0.0
echo code > pkg/a.go && git add -A && git commit -qm "feat(mqtt)!: breaking"
run "breaking proposes major" "Proposed version: v2.0.0" yes

newrepo
echo code > pkg/a.go && git add -A && git commit -qm "chore: more init"
run "first release always proposes" "Proposed version: v0.0.1" yes

newrepo; git tag v1.0.0
mkdir -p .github && echo y > .github/wf.yml && git add -A && git commit -qm "feat(ci): ci-only feature"
echo mod > go.mod && git add -A && git commit -qm "chore(deps): bump toolchain"
run "ci-only feat does not force immediacy" "accumulating" no

newrepo; git tag v1.0.0
mkdir -p .github && echo y > .github/wf.yml && git add -A && git commit -qm "feat(ci): ci-only feature"
echo mod > go.mod && git add -A
GIT_COMMITTER_DATE=$(aged) git commit -qm "chore(deps): bump toolchain"
run "ci-only feat does not inflate bump" "Proposed version: v1.0.1" yes

# --- shipped-path lists stay complete ---
# Every top-level Go source file must reach the image (.dockerignore is an
# allow-list) and count as shipped for releases and notes; a new file missing
# from either silently breaks the build or skips a release.
REPO_ROOT="$(dirname "$SCRIPT")/.."
for f in "$REPO_ROOT"/*.go; do
    name=$(basename "$f")
    case $name in *_test.go) continue ;; esac
    grep -qxF "!/$name" "$REPO_ROOT/.dockerignore" || { echo "FAIL: $name missing from .dockerignore"; exit 1; }
    for s in release-agent.sh release-notes.sh; do
        grep -qE "^SHIPPED_PATHS=\(.*\b$name\b" "$REPO_ROOT/scripts/$s" || { echo "FAIL: $name missing from SHIPPED_PATHS in $s"; exit 1; }
    done
done
echo "PASS: every top-level Go file is shipped"

# --- release-notes.sh ---

# notes_has <name> <file> <substring>...: every substring must appear.
notes_has() {
    local name=$1 file=$2
    shift 2
    for want in "$@"; do
        if ! grep -qF -- "$want" "$file"; then
            echo "FAIL: $name - missing: $want"; cat "$file"; exit 1
        fi
    done
    echo "PASS: $name"
}

newrepo; git tag v1.0.0
echo a > pkg/a.go && git add -A && git commit -qm "feat(mqtt): shiny feature"
echo b > pkg/b.go && git add -A && git commit -qm "fix(dahua): squash a bug"
echo c > pkg/c.go && git add -A && git commit -qm "perf: faster"
echo mod > go.mod && git add -A && git commit -qm "chore(deps): bump things"
echo d > pkg/d.go && git add -A && git commit -qm "refactor: tidy" -m "BREAKING CHANGE: config renamed"
echo e > pkg/e.go && git add -A && git commit -qm "test: more tests"
mkdir -p .github && echo y > .github/wf.yml && git add -A && git commit -qm "feat(ci): ci-only feature"
bash "$NOTES_SCRIPT" v1.0.0 HEAD v1.1.0 > notes.md
notes_has "notes group by type" notes.md \
    "## Features" "- shiny feature (" "## Fixes" "- squash a bug (" "## Performance" "- faster (" \
    "## Dependencies" "- bump things (" "## ⚠️ Breaking changes" "- tidy (" "## Other changes" "- more tests (" \
    "docker pull ghcr.io/will-white/dahua-companion:v1.1.0" "compare/v1.0.0...v1.1.0"
if grep -q "ci-only feature" notes.md; then
    echo "FAIL: notes list a CI-only commit"; exit 1
fi
echo "PASS: notes skip unshipped commits"

if bash "$NOTES_SCRIPT" v9.9.9 HEAD > /dev/null 2>&1; then
    echo "FAIL: notes accepted an unknown ref"; exit 1
fi
echo "PASS: notes reject an unknown ref"

newrepo; git tag v1.0.0
echo code > pkg/a.go && git add -A && git commit -qm "feat(mqtt): new thing"
run "proposal carries grouped notes" "Proposed version: v1.1.0" yes
notes_has "proposal shows the notes" release_proposal.md "## Features" "- new thing (" "**Commit:**"

echo "ALL PASS"
