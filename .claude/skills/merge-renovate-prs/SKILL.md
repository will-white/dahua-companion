---
name: merge-renovate-prs
description: >-
  Triage and merge the repo's open dependency PRs (Renovate bumps for GitHub
  Actions, the golang Docker base image, the Go toolchain directive, and Go
  modules) safely, in bulk. Use when the user asks to "go through the PRs",
  "merge the PRs", "clear the Renovate backlog", "update dependencies", or
  similar. Handles the docker-publish.yml single-file pile-up, duplicate
  major/minor PRs, researches breaking changes on majors, and holds
  release-pipeline changes that CI cannot verify.
---

# Merge Renovate / dependency PRs

This is a single Go binary repo (`will-white/dahua-companion`). Nearly all open
PRs are Renovate bumps of GitHub Actions, the `golang` builder image, the Go
toolchain directive, or Go modules. Goal: merge everything safe, resolve
duplicates to the highest appropriate version, apply code changes a major
requires, and **stop and ask** before anything that can only break at release
time.

Merging to `main` deploys nothing — the image is built, pushed, and signed only
when a release is cut (see `CLAUDE.md` → Release flow). So there is no
live-service risk here; the risk is shipping a broken *release pipeline* that
stays invisible until the next tag.

## 0. Preflight

- **`gh` is not installed in Linux. Use `gh.exe`** (Windows GitHub CLI, reachable
  from WSL at `/mnt/c/Program Files/GitHub CLI/gh.exe`, already on `PATH`).
- **Always pass `--repo will-white/dahua-companion`.** `gh.exe` cannot
  autodetect the repo — it shells out to Windows `git`, which rejects the WSL
  path with *"detected dubious ownership … //wsl.localhost/…"*. Every bare
  `gh.exe pr list` fails this way. Do not "fix" it by adding a global
  `safe.directory` to the Windows git config; just pass `--repo`.
- Confirm auth: `gh.exe api user -q .login` → `will-white`. Token scopes already
  include `repo` and `workflow` (needed — most PRs here edit
  `.github/workflows/*`).
- `main` is **unprotected** (`gh.exe api repos/<repo>/branches/main/protection`
  → 404), so red checks do not block merging. That is a licence to use judgment,
  not to ignore CI.
- Repo merge settings: **squash and rebase only** (merge commits disabled),
  branches auto-delete, auto-merge disabled. History is linear — keep it that way.
- Prefer `--body '...'` inline over `--body-file`; `gh.exe` reads paths as
  Windows paths and a WSL path may not resolve.
- **`git fetch origin` first.** Renovate merges land without touching the local
  clone, so local `main` is routinely many commits behind — it was 6 behind on
  2026-08-11. Reading a workflow from a stale worktree gives you the wrong
  action versions and invents conflicts that do not exist.

## 1. Survey every open PR

```bash
R=will-white/dahua-companion
gh.exe pr list --repo $R --state open --limit 100 \
  --json number,title,mergeable,mergeStateStatus,isDraft,author \
  --jq '.[] | "\(.number)\t\(.author.login)\t\(.mergeable)\t\(.mergeStateStatus)\t\(.title)"' | sort -n
```

Build the **file-conflict map** — it drives merge order:

```bash
for pr in <all numbers>; do
  echo "$pr :: $(gh.exe pr view $pr --repo $R --json files --jq '[.files[].path]|join(" ")')"
done
```

Expect the map to be lopsided: **most Action bumps touch only
`.github/workflows/docker-publish.yml`**, because that is the one workflow with
SHA-pinned actions. That single file is the main source of merge conflicts here
(§5).

## 2. Check *which* check is red before assuming the PR broke it

```bash
gh.exe pr checks <pr> --repo $R
```

Two checks gate a PR: **`Test & Lint`** (build, vet, `gofmt -l`,
`go test -race`) and **`govulncheck`**. Since 2026-08 a third, the build-only
**Docker** run, gates PRs too (§5).

`govulncheck` was red on every PR for roughly a month (2026-07 → 2026-08-11)
and was **inherited from `main`, not caused by the PRs**. Fixed in #62/#63
(Go toolchain → 1.26.5) and #75 (`golang.org/x/net` → v0.57.0). If it goes red
again, diagnose before touching code — the two recurring causes are:

- **Standard-library CVEs** (`crypto/tls`, `crypto/x509`, `net`, `net/http`,
  `net/textproto`, `os`) reported against the Go version CI resolves from
  `go.mod`. Fixed by **bumping the toolchain, not by editing code**. Bump the
  `go.mod` toolchain and the `Dockerfile` builder tag **together** (§3).
- **Indirect module deps.** `golang.org/x/net` reaches this repo via
  `paho.mqtt.golang` → `gorilla/websocket`. Renovate does not open PRs for
  indirect deps, so these need a manual bump and will never appear in the
  backlog:

  ```bash
  go get golang.org/x/net@<fixed-version> && go mod tidy && go test -race ./...
  ```

  Note this can raise the `go` directive (v0.57.0 forced `1.24.0` → `1.25.0`).
  Land it as its own PR; do not fold it into a Renovate branch.

Because `go` is not installed locally, run these in the official image — see
`CLAUDE.md` → Commands for the exact `docker run` invocation.

## 3. Classify

- **True duplicates** = two PRs editing the **same line** of the same file for
  the same action — Renovate routinely opens a minor *and* a major for one
  action (e.g. `docker/login-action v3.7.0` alongside `docker/login-action v4`;
  `docker/build-push-action v6.19.2` alongside `v7`). Keep the **highest**
  version that §4 clears, **close the other** with a note.
- **NOT duplicates (merge both, as a pair)**: the Go version is expressed in two
  places — `toolchain` in `go.mod` and the `golang:<ver>-alpine` builder tag in
  `Dockerfile`. Renovate splits these into separate PRs. **Merge them together**
  so the CI toolchain and the release image do not drift apart.
- **Majors** (whole-number jump, or `!` in the title): research per §4.
- **Non-Renovate PRs** (past bots here: `copilot-swe-agent`,
  `google-labs-jules`): review individually, never batch.

Note the two pinning styles — `docker-publish.yml` pins actions by **commit SHA**
with a trailing `# vX.Y.Z` comment, while `ci.yml`, `security.yml`,
`release-agent.yml`, and `release-publish.yml` use plain `@vN` tags. A bump to a
widely-used action like `actions/checkout` touches **all five workflow files** at
once; verify the SHA *and* the version comment both moved.

## 4. Research breaking changes on majors

Fan out one general-purpose agent per major (parallel). Each must read the
affected workflow, fetch upstream release notes, and cross-reference **the
inputs this repo actually sets** against removed/renamed ones — reporting
`file:line` edits needed. Points to check for this repo specifically:

- `docker/build-push-action` — `cache-from`/`cache-to: type=gha` and the
  `outputs.digest` used by the cosign step.
- `docker/metadata-action` — the `tags:` block uses `type=semver`, `type=raw` and
  `enable=` conditions; schema changes here break tagging silently.
- `sigstore/cosign-installer` — `cosign-release` is pinned to `v2.2.4`; a major
  may drop that input or change the signing invocation.
- `actions/checkout` — the `ref: ${{ inputs.version || github.ref }}` in
  docker-publish and `fetch-depth: 0` in release-agent must survive.
- Go module majors need a **code** change, not just `go.mod` — the
  `cenkalti/backoff` v4→v5 bump is the precedent (`backoff.Retry` changed
  signature; see `pkg/dahua/dahua.go`). Run `go build ./...` and
  `go test -race ./...` locally on the branch before merging one of these.

## 5. HOLD and ASK (do not auto-merge)

Surface with `AskUserQuestion` before merging:

1. **The release-only half of `docker-publish.yml`.** Since 2026-08 the workflow
   runs on PRs in build-only mode, so a broken `Dockerfile` or a bad
   build-push/buildx/metadata bump now fails the PR. But the steps guarded by
   `if: github.event_name != 'pull_request'` — **registry login, the actual
   push, cosign install, and image signing** — still run for the first time at
   release. Treat `sigstore/cosign-installer` and `docker/login-action` majors
   as unverified, and read the `outputs.digest` / tag wiring by hand.
2. **`docker/metadata-action` majors** — the build is validated on PRs, but the
   `type=semver` / `type=raw` tag *values* only matter on a real tag. A schema
   change can green the PR and still mis-tag or drop `latest` at release.
3. **Go module majors requiring a refactor** (§4) — merge only after the code
   change is in the same PR and `go test -race ./...` passes.

Everything else — Action minors/patches, Go toolchain patch bumps, module
minors — is safe to batch.

## 6. Merge

```bash
gh.exe pr merge <pr> --repo $R --squash
gh.exe pr view <pr> --repo $R --json state,mergedAt   # verify; gh is quiet on success
```

Order matters because of the `docker-publish.yml` pile-up:

1. Merge the **Go toolchain + Dockerfile pair** first — it is the govulncheck
   fix, and it touches files nothing else touches.
2. Merge the **unique-file** PRs freely.
3. Then work the `docker-publish.yml` cluster **one at a time**. GitHub
   3-way-merges distinct lines cleanly, so siblings usually stay `MERGEABLE`,
   but after each merge re-check the rest and run
   `gh.exe pr update-branch <pr> --repo $R` on any that flip to `CONFLICTING`.

**`update-branch` before merging anything in the docker cluster.** A PR runs the
workflows *as they exist on its own branch*, so a Renovate branch cut before a
workflow change does not have the new check at all — after the PR-trigger landed
on 2026-08-11, every open PR still showed only `Test & Lint` and `govulncheck`,
and would have merged with **zero** docker validation. `update-branch` pulls in
the trigger and the `build` check appears. Same reasoning applies to validating
the *combined* result: refresh again after a batch of merges so the last PRs are
built against a base that already has the others.
4. Close the losing half of each duplicate pair with a note naming the PR that
   superseded it.

If Renovate **auto-closes** a PR whose branch conflicted after you merged a
same-file sibling (`state: CLOSED, mergedAt: null`) and the bump is still wanted,
recreate it: branch off `main`, re-apply the one-line change (grab the target SHA
from `gh.exe pr diff <old-pr> --repo $R`), push, open a PR, merge.

### Gotchas

- Shell is **bash** — normal word-splitting, no fish workarounds needed.
- `go` is **not on PATH** in this WSL environment. If a check needs
  `go build`/`go test`, ask the user to run it (`! go test -race ./...`) rather
  than assuming it is available.
- Never `git push origin main` — branch, PR, merge, even though main is
  unprotected.
- Renovate config is bare `config:recommended`; dependabot separately handles
  only `devcontainers`. A bump appearing from `dependabot[bot]` for
  `.devcontainer/devcontainer.json` is expected and independent.

## 7. Verify

```bash
gh.exe run list --repo $R --branch main --limit 10 \
  --json workflowName,conclusion,headSha \
  --jq '.[] | "\(.conclusion)\t\(.workflowName)\t\(.headSha[0:7])"'
```

`Test & Lint` must be green. Re-check `govulncheck` after the toolchain merge —
report the remaining count and whether `golang.org/x/net` is still outstanding.
Nothing deploys, so there is nothing else to observe.

## 8. Report

Summarize: merged count, duplicates closed (with which version won), breaking
changes fixed, held PRs **with the reason for each**, and the post-merge
govulncheck state. Offer to open the follow-up `golang.org/x/net` PR and to cut a
release if the pipeline changed.
