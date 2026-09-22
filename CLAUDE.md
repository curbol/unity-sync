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
- `config` — user settings by precedence: defaults → `config.toml` → env → flags. A config
  dir the user named (`--config`, `$UNITY_SYNC_CONFIG_DIR`) must exist; the XDG and
  `~/.config` fallbacks need not, since no config file is the ordinary first run. Absent,
  the two are indistinguishable, and a misspelled one silently drops `library_path` and
  mirrors tens of gigabytes into the default directory. `--manifest` gets the same rule
  and the same `ExpandHome`, applied in `main.namedManifest`: an absent manifest loads as
  an empty allowlist, so a typo mirrors nothing and still exits 0. A `concurrency` the
  file sets below 1 is refused the way `--concurrency 0` is, since the merge guard cannot
  tell a zero someone typed from a key nobody wrote.
- `session` — builds the Cookie header from a Firefox-family session store, a pasted curl
  file, or a `cookies.txt`, and asserts the `LS` cookie is present before any request. The
  source is identified by reading it, not by configuration. Both the running browser's
  `recovery.jsonlz4` and the `sessionstore.jsonlz4` a clean exit leaves behind are
  searched, live files first across every root. The profile the running installation uses
  comes from an `[Install<hash>]` section, which Mozilla keeps in `profiles.ini` as well as
  in `installs.ini`: both are read, `profiles.ini` first, because the copy can be absent or
  stale and ranking then falls back to the `Default=1` flag, which is the ordering this
  exists to override. `mozlz4.go` decodes Gecko's
  compressed session store; the jar it holds spans every host the browsing session touched,
  so it is filtered to `unity.com` before anything leaves the package.
- `retry` — backoff policy. `retry.Permanent` lets a caller stop on a body-based verdict
  that the status code alone would have retried.
- `unitypackage` — reads the store descriptor from a package's gzip FEXTRA field.
- `store` — the Asset Store client and the response-level download guards.
- `cache` — the local mirror. Two-phase writes (`Store` → `Commit`/`Discard`), adopt by
  scan, relocate on rename, temp sweep, root confinement. Every path here that creates
  directories unwinds them when it then fails, `Relocate` included, or a rename that loses
  a race leaves an empty `<publisher>/<asset>/` in the tree quarry walks, on every run.
  `Canonical`/`SamePath` are the only correct way to compare a lockfile-recorded path
  against a derived one, and `SameFile` is what pairs with them where the filesystem
  ignores case.
- `lockfile` — `unity-sync.lock.json`, advertised fields kept apart from the embedded
  `Resolution`, which is one type so its two write paths cannot drift. It is embedded
  *last* on purpose: `encoding/json` emits an embedded struct's fields at its own index, so
  moving it shifts every key in every entry and the next sync rewrites the whole committed
  file.
- `manifest` — `unity-sync.toml`, the committed allowlist keyed by asset id.
- `syncer` — orchestration, the pure `classify`, and the semantic download guards.
  `Classes()` and `Class.String()` must both name every class: `main` drives the whole
  per-class tally off the first, and the second's default arm answers "unchanged", so a
  class missing from either is reported as a no-op and dropped from a tally that still
  counts it in the total.
- `humanize` — byte sizes for people, clamped: the count comes from the store.
- `web` — the `select` page.
- `selfupdate` — the `update` subcommand.
- `fixtures` + `cmd/scrubfixtures` — regenerate PII-free `testdata/` from raw captures.

### Key invariants (don't break these)

- **`LS` is the credential.** Not the NextAuth session token, which neither endpoint
  consults. Its absence is reported before any request, because the store answers a
  missing `LS` with an opaque 500. It is absent from `cookies.sqlite` but present in a
  Gecko session store, which is what makes the browser source possible. A pasted curl
  command is unquoted the way the shell it was copied for quoted it — POSIX, ANSI-C
  `$'…'`, and the Windows cmd form with its `^"` wrapper and caret escapes — because
  matching one quote style drops the credential from the others. The flag is read the same
  way: both `-H 'cookie: …'` and curl's own `-b '…'`, whose value is the cookie string
  rather than a header, and where a value holding no `=` is a jar filename and not a
  credential.
- **No cookie value is ever logged**, and a session store is filtered to the `unity.com`
  family inside `internal/session`. That file carries credentials for every host the
  browsing session touched. Narrowing to a host is not narrowing to an account:
  Multi-Account Containers gives a container tab its own jar, so one file can hold two `LS`
  cookies for the same host under two Unity accounts. The default context is taken first
  and a container fills only the names it did not supply, so a container-only sign-in still
  resolves and a container never silently outranks the rest of the browser.
  `Resolved.String` is `Path`, so the default way to print one is the safe one.
- **No store client follows a redirect.** An unauthenticated download 302s to Unity's
  OAuth page. `selfupdate` is the deliberate exception: it talks to GitHub, whose asset
  API 302s to a signed CDN URL by design.
- **`store.Fetch` marks its own sentinels permanent.** A pulled asset and an expired
  session must not be retried by a caller that did not think to convert them.
- **Every store call retries on the same terms, the bootstrap included.** It is the first
  request a run makes, so a 5xx there would otherwise end the run before any work was done
  while the identical fault one call later gets a full schedule. A route that answers
  *without* a token is not retried: that status is not retryable and proceeding guarantees
  `ErrCSRF`. The mid-run re-bootstrap stays deliberately single-shot, which is why `search`
  calls `bootstrapOnce` rather than `Bootstrap`.
- **Downloads ask for `Accept-Encoding: identity`.** The endpoint honours gzip by
  gzipping the already-gzipped package, and Go will not decode an encoding the caller
  requested.
- **A download body carries a stall guard, not a deadline.** A 23 GB package legitimately
  takes hours, so there is no whole-request timeout. A body that goes silent after its
  headers arrive would otherwise block the read forever: the attempt never returns, so the
  retry that would open a fresh connection never runs and the pool slot is never given up.
  The API calls carry small JSON and are bounded end to end instead. The guard rides on the
  body `Fetch` hands back, so a *rejected* response — whose body is drained rather than
  abandoned, to return the connection to the pool — reads under no deadline at all; that
  drain is bounded by the API call's own, or a captive portal that answers 200 `text/html`
  and goes quiet wedges the run through the one door the guard is not on.
- **`resolvedVersionId` is the diff key**, not the advertised `version.id`. The advertised
  value refreshes every run; pairing a refreshed id with an unresolved entry's file would
  mark it current forever. Two entries for one asset id are refused at load — by the
  manifest as well as the lockfile: a rename re-keys an entry, so a merge can leave both,
  and lookups walk the map — the run would pick between them at random and drop the one it
  did not pick.
- **A derived slug is always a usable directory name.** `PublisherSlug` carries no id
  suffix, so it is the one segment that can come out a bare word: a publisher whose name
  folds to a Windows device name (`con`, `aux`, `com1`…) falls back to the id, and
  `cache.safeSegment` refuses one that arrives any other way. `MkdirAll` fails on those
  names, so without this the asset fails on Windows and nowhere else.
- **A run stops classifying when its context ends.** The pass that hashes, relocates and
  deletes is where a large run spends its time, and `main`'s signal handler has already
  disabled SIGINT's default action, so an uninterruptible pass cannot be escalated out of
  either. It breaks rather than returns, so the tail still records what was resolved;
  `cache.Scan` and `cache.SweepTemps` stop mid-walk for the same reason.
- **A recorded `cachePath` is compared with `cache.SamePath`, never `==`.** That file is
  committed and hand-editable, so two spellings name one file; comparing them raw makes a
  run delete the package it just downloaded as a superseded copy. `cache.Canonical` works
  in slash space and refuses a backslash or a colon: Windows' `filepath.Clean` lifts a
  volume prefix out before resolving `..` and restores it after, so `Z:../../x` cleans to
  itself and escapes the root on that platform alone. Spelling is not the whole answer
  where the filesystem ignores case, so every delete or move asks `cache.SameFile` too —
  `cache.Relocate` included, where an occupant that is the source under its other spelling
  is a no-op rather than the refusal that would fail the adopt on those platforms forever.
- **Confinement is the filesystem's, not the string's.** `Canonical` settles a spelling,
  and a path whose every segment is an ordinary name still leaves the library when one of
  them is a symlink. Every operation that opens, creates, moves or
  removes goes through `os.Root` — `cache.rooted` for a lockfile-supplied path, and an
  `os.OpenRoot` inside `Store`, `Commit` and `Discard` for the ones a run derives — which
  resolves inside the root at the syscall level; `resolve` stays lexical and is only for
  the callers that want the name rather than the file. The writes belong in that set as
  much as the reads: held to `Canonical` alone they agreed about spelling and disagreed
  about symlinks, with the write as the permissive side, so a symlinked publisher directory
  was writable and then unreadable and the asset re-downloaded in full forever, reported
  only as `cache-missing`. `pruneEmptyParents` goes through the root too, or walking up
  removes the user's link and leaves the directory it pointed at. `Scan` and `SweepTemps`
  walk `rt.FS()` rather than the path for a different reason: `filepath.WalkDir` opens with
  an `Lstat` and stops at anything that is not a directory, so a `library_path` that is
  itself a symlink — a Windows junction included — made both visit the link and descend
  nothing, silently, while every other operation kept working. A symlinked directory
  *inside* the library is still not descended, which is the half the write gate relies on.
- **The lockfile is rewritten as each asset resolves, in every pass.** Not only the
  downloads: the classification pass relocates and deletes whole packages, so a run that
  adopts and fetches nothing would otherwise ride on one closing write. Lose it and the
  library has moved while the record names the old path with a digest that no longer
  matches, which the next run reads as `Unchanged` and carries forward. The flush before
  the rename is what makes that record durable, and `syncFile` is indirected so a test can
  hold `Save` to it: nothing observable distinguishes a save that skipped it until the
  machine loses power. A kill between the create and the rename orphans a temp in the
  directory the user commits, so a run sweeps those alongside the cache's.
- **Nothing unverified reaches a real cache path.** `cache.Store` does not rename;
  `Commit` does, after the syncer's guards pass. `Store` also refuses every path a later
  read would refuse — one `Canonical` will not resolve, and one that leaves the library
  through a symlink — so the write gate cannot be weaker than the read gate in either of
  the two ways a path escapes.
- **The adoption gates run inside the scan's own selection**, not on the candidate it
  hands back. `cache.Index.Find` returns one file, preferring the copy already at the
  derived path, so gates applied afterwards rejected that copy while another that would
  have passed sat unexamined: a stale or truncated build where the layout puts it masks an
  intact one elsewhere and the asset re-downloads in full. A cloud sync client's conflicted
  copy is the ordinary way a library comes to hold two.
- **The size floor is asked before the descriptor guards.** The descriptor is the first
  ~350 bytes, so a transfer that dropped inside them does not parse as gzip at all — which
  the descriptor guard marks `retry.Permanent` while the identical fault a few hundred
  bytes later reaches the floor and is retried. A short body's diagnosis is "truncated",
  which is retryable and has its own discriminator.
- **The temp sweep's grace is derived from the stall window, not chosen.** A transfer is
  alive as long as `store.DefaultStallTimeout` tolerates its silence, so a shorter grace
  unlinks a temp the run that owns it is still going to write to; it then finishes into a
  deleted file and fails permanently, because an opened-but-deleted temp reads as an
  unparseable package rather than a truncated one.
- **A delisted asset already in the library is adopted, not reported missing.** A disabled
  product answers 404, so it is the one class where a download cannot make up the
  difference and the lockfile is not the only thing that knows the bytes are here.
- **An asset whose bytes landed and whose entry did not keeps the exit status non-zero.**
  All three persisting paths — download, adoption, the relocation a rename forces — report
  it as a warning naming the asset rather than as a failure of it, since the work was done
  and the record of it was lost. Two of them used to warn and exit 0 while the third called
  it a failure: the lockfile is what the next run reads, so a relocation it does not know
  about classifies `CacheMissing` and re-downloads in full on every run.
- **A failed download fails its asset, not the run**, and a pulled asset does not make the
  run exit non-zero. Assets a cancelled pool never reached are counted apart from the ones
  that failed and summarised in one line, so an expired session does not bury its own
  diagnostic under a cancellation per remaining asset; they still exit non-zero.
- **The select page is served only to a browser on this machine.** The bind address is
  refused unless it names one address, because `Host` is client-supplied and a wildcard
  bind has nothing to check it against. Every request's `Host` is then checked against the
  bound address, before the render as well as before a save — and `web.localRequest`
  refuses an unspecified bound address itself rather than trusting `main` to have done it,
  because `Serve` takes any `net.Addr` and the two tests it would otherwise reach
  (`Host: localhost`, a loopback literal) are true of a request from anywhere. The
  per-run token stops a blind cross-origin POST but not DNS rebinding, which the browser
  treats as same-origin *by name*: without the check, a page the user is already on could
  read the whole owned-asset list and spend the one save this page accepts, leaving the
  user's own save refused as a stale tab.
- **Only `select` writes the manifest.** `status` and `sync` read it. `manifest.Reconcile`
  refuses an owned set that is empty, and one that shares no id with what was enabled: both
  are what a wrong-org session looks like, and the select page's own would-empty guard is
  compared against a set `Reconcile` has already rewritten. A second save is refused as a
  second save, not as a stale tab: its token is current, and `Serve` has returned, so
  telling that user to reload is both false and impossible.
- **An update installs nothing that is not a native binary**, and it sends the GitHub
  token only to the API host it was pointed at, never to a URL a response named. The zip
  reader verifies each entry's CRC; a magic-byte check catches the other failure, a release
  that shipped an error page or the wrong artifact under the right name. It runs *before*
  the rename, in `selfupdate` and in `install.sh` alike, because past that point the
  working binary is gone and leaving nothing usable on PATH is the one outcome an updater
  must never produce. The token is only an optimisation — everything read is public — so
  one the API rejects (401, 403, 404) is retried anonymously rather than failing the
  update, and `gh auth token` is asked for `github.com` by name so an enterprise-only
  login is not sent to `api.github.com`. That marker decides whether to retry and never what
  to report: only one of the three statuses ever means "this credential", so a 404 for a
  version that was never tagged is reported as a 404. `install.sh` needs the status for
  that as much as the updater does, since `curl -f` fails alike on every status at or above
  400 and on every transport failure — keyed on its exit status the installer blamed the
  credential for a 502, a proxy reset and a DNS failure alike. It also refuses to attach a
  credential to an `API_BASE` that is not GitHub's: that override is a test seam, and
  otherwise a way to turn "can set an environment variable" into "has this user's token".
  A refused update reads no credential at all — the dev-build check comes before the
  lookup, or `go test` spawns `gh auth token` on the developer's machine. CI refuses a `uses:` naming a tag, and
  walks the whole of `.github/` rather than `workflows/`, because a local composite action's
  own pins run in the same job.
- **No account data in the repo.** Sessions and raw captures stay out; the
  `internal/fixtures` guard test fails the build if any reaches *any* `testdata/`, package
  local ones included. The scrub is an allowlist projected from `store.SearchDocument`, so
  a field the query never asked for cannot reach a fixture — and the committed fixtures are
  held to being that scrubber's output by re-scrubbing each and comparing bytes, because
  the forbidden-field list names five fields while the allowlist is what decides. CI also
  refuses a tracked compiled binary: nothing else here looks at the tracked file set, and a
  6.8 MB artifact left at the root by `go build ./cmd/scrubfixtures` was committed once
  already.

## Editing testdata

Don't hand-edit `testdata/store/*.json`. They are generated by
`go run ./cmd/scrubfixtures`, which scrubs raw API captures — it reads `captures/` by
default, git-ignored because every captured row carries an entitlement id. The captures are
not in the repo and are not reproducible without a signed-in session, so regenerating means
capturing again. Regenerate rather than patch, and keep the guard test green.
