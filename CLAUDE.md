# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`unity-sync` is a Go CLI that mirrors the assets owned on the Unity Asset Store into a
local library, downloading only what changed since the last run. See `README.md`
(user-facing) and `docs/design.md` (the authoritative design doc: measured store
behaviour, identity rules, the failure model). Read `docs/design.md` before changing
enumeration, the lockfile, the cache layout, or the download guards.

Browsing the mirrored library is a separate tool,
[quarry](https://github.com/curbol/quarry). This repo acquires files; quarry reads them.

## Build & test

```bash
go build -o unity-sync .        # requires Go 1.26+, no cgo
go test ./...                   # full suite, fully offline
go test -race ./...             # what CI runs
go test ./internal/syncer/ -run TestClassify -v
go vet ./...
gofmt -l .
```

No Makefile or task runner; use the `go` toolchain directly. The suite needs no network
and no session: everything runs against `httptest` servers and the committed, scrubbed
fixtures in `testdata/store/`.

## Architecture

`main.go` `run()` parses flags and dispatches `select`, `status`, `sync`, `list`, `update`
and `version`, returning an exit code alongside its error. Layered `internal/` packages,
each with a package doc comment stating its contract:

- `model` — domain types and the identity rules. Carries `id` (the store product id) and
  deliberately not `productId`, which is a different value no endpoint accepts.
- `config` — user settings by precedence: defaults → `config.toml` → env → flags.
- `session` — builds the Cookie header from a Firefox-family session store, a pasted curl
  file, or a `cookies.txt`, and asserts the `LS` cookie is present before any request. The
  source is identified by reading it, not by configuration. `mozlz4.go` decodes Gecko's
  compressed session store; the jar it holds spans every host the browsing session touched,
  so it is filtered to `unity.com` before anything leaves the package.
- `retry` — backoff policy. `retry.Permanent` lets a caller stop on a body-based verdict
  that the status code alone would have retried.
- `unitypackage` — reads the store descriptor from a package's gzip FEXTRA field.
- `store` — the Asset Store client and the response-level download guards.
- `cache` — the local mirror. Two-phase writes (`Store` → `Commit`/`Discard`), adopt by
  scan, relocate on rename, temp sweep, root confinement. `Canonical`/`SamePath` are the
  only correct way to compare a lockfile-recorded path against a derived one.
- `lockfile` — `unity-sync.lock.json`, advertised fields kept apart from resolution fields.
- `manifest` — `unity-sync.toml`, the committed allowlist keyed by asset id.
- `syncer` — orchestration, the pure `classify`, and the semantic download guards.
- `humanize` — byte sizes for people, clamped: the count comes from the store.
- `web` — the `select` page.
- `selfupdate` — the `update` subcommand.
- `fixtures` + `cmd/scrubfixtures` — regenerate PII-free `testdata/` from raw captures.

### Key invariants (don't break these)

- **`LS` is the credential.** Not the NextAuth session token, which neither endpoint
  consults. Its absence is reported before any request, because the store answers a
  missing `LS` with an opaque 500. It is absent from `cookies.sqlite` but present in a
  Gecko session store, which is what makes the browser source possible.
- **No cookie value is ever logged**, and a session store is filtered to the `unity.com`
  family inside `internal/session`. That file carries credentials for every host the
  browsing session touched.
- **No store client follows a redirect.** An unauthenticated download 302s to Unity's
  OAuth page. `selfupdate` is the deliberate exception: it talks to GitHub, whose asset
  API 302s to a signed CDN URL by design.
- **`store.Fetch` marks its own sentinels permanent.** A pulled asset and an expired
  session must not be retried by a caller that did not think to convert them.
- **Downloads ask for `Accept-Encoding: identity`.** The endpoint honours gzip by
  gzipping the already-gzipped package, and Go will not decode an encoding the caller
  requested.
- **A download body carries a stall guard, not a deadline.** A 23 GB package legitimately
  takes hours, so there is no whole-request timeout. A body that goes silent after its
  headers arrive would otherwise block the read forever: the attempt never returns, so the
  retry that would open a fresh connection never runs and the pool slot is never given up.
  The API calls carry small JSON and are bounded end to end instead.
- **`resolvedVersionId` is the diff key**, not the advertised `version.id`. The advertised
  value refreshes every run; pairing a refreshed id with an unresolved entry's file would
  mark it current forever.
- **A derived slug is always a usable directory name.** `PublisherSlug` carries no id
  suffix, so it is the one segment that can come out a bare word: a publisher whose name
  folds to a Windows device name (`con`, `aux`, `com1`…) falls back to the id, and
  `cache.safeSegment` refuses one that arrives any other way. `MkdirAll` fails on those
  names, so without this the asset fails on Windows and nowhere else.
- **A recorded `cachePath` is compared with `cache.SamePath`, never `==`.** That file is
  committed and hand-editable, so two spellings name one file; comparing them raw makes a
  run delete the package it just downloaded as a superseded copy.
- **Nothing unverified reaches a real cache path.** `cache.Store` does not rename;
  `Commit` does, after the syncer's guards pass.
- **A failed download fails its asset, not the run**, and a pulled asset does not make the
  run exit non-zero.
- **The select page is served only to a browser on this machine.** Every request's `Host`
  is checked against the bound address, before the render as well as before a save. The
  per-run token stops a blind cross-origin POST but not DNS rebinding, which the browser
  treats as same-origin *by name*: without the check, a page the user is already on could
  read the whole owned-asset list and spend the one save this page accepts, leaving the
  user's own save refused as a stale tab.
- **Only `select` writes the manifest.** `status` and `sync` read it. `manifest.Reconcile`
  refuses an owned set that is empty, and one that shares no id with what was enabled: both
  are what a wrong-org session looks like, and the select page's own would-empty guard is
  compared against a set `Reconcile` has already rewritten.
- **An update installs nothing that is not a native binary.** The zip reader verifies each
  entry's CRC; a magic-byte check catches the other failure, a release that shipped an
  error page or the wrong artifact under the right name. It runs *before* the rename, in
  `selfupdate` and in `install.sh` alike, because past that point the working binary is
  gone and leaving nothing usable on PATH is the one outcome an updater must never produce.
- **No account data in the repo.** Sessions and raw captures stay out; the
  `internal/fixtures` guard test fails the build if any reaches *any* `testdata/`, package
  local ones included. The scrub is an allowlist projected from `store.SearchDocument`, so
  a field the query never asked for cannot reach a fixture.

## Editing testdata

Don't hand-edit `testdata/store/*.json`. They are generated by
`go run ./cmd/scrubfixtures`, which scrubs raw API captures — it reads `captures/` by
default, git-ignored because every captured row carries an entitlement id. The captures are
not in the repo and are not reproducible without a signed-in session, so regenerating means
capturing again. Regenerate rather than patch, and keep the guard test green.
