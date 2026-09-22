# unity-sync design

`unity-sync` mirrors the assets owned on the Unity Asset Store into a local library,
detecting and downloading only what changed. This document is the authoritative record of
the store's behaviour, the identity rules, and the failure model. Read it before changing
enumeration, the lockfile, the cache layout, or the download guards.

## Goals

- One command pulls every selected asset to its current version with no clicking.
- Detect updates without downloading: the list API carries each asset's current version.
- A committed lockfile is the record of what is owned and at what version, whose monthly
  diff reads like a changelog.
- The cache is local and expendable; the assets a project actually ships are made durable
  in that project, not here.
- Resilient to what actually breaks: an expired session, a delisted asset, a truncated
  transfer, a renamed asset.

## Non-goals

- Importing or unpacking `.unitypackage` into a Unity project.
- Converting or previewing assets. Browsing the mirror is [quarry](https://github.com/curbol/quarry).
- Scripting the Unity ID login. The session is handed in.

## What the store exposes (measured)

| Purpose | Call |
| --- | --- |
| Owned-asset list | `POST /api/graphql/batch`, operation `SearchMyAssets` |
| Single-product re-read | the same document with `ids: ["<id>"]` |
| Package bytes | `GET /api/downloads/{id}` |

The GraphQL body is a batch: a JSON array of operations, answered by a positional array.

### Authentication

`LS` is the entire credential. Measured cookie-by-cookie: `_csrf` + `LS` alone returns the
full owned list; a junk or absent `__Secure-next-auth.session-token` changes nothing;
removing `LS` turns any user-scoped query into an HTTP 500 with an empty `GraphqlError`.

`LS` is a session cookie, so it never reaches `cookies.sqlite`. It does reach a
Firefox-family **session store**, which is a different file for a different purpose:
Gecko's `sessionstore-backups/recovery.jsonlz4` records the cookies of every host the
browsing session touched so the session can be restored. Measured on a real profile: `LS`
and `_csrf` are both in there, and the two of them alone return HTTP 200 with the full
owned list.

Two properties decide what that can promise. The jar is **not tab-scoped** — on the profile
measured, 156 of 169 cookie hosts had no tab open anywhere in the session, and
`assetstore.unity.com` was one of them — so the credential survives closing the tab and
lasts as long as the browsing session. And the file is rewritten **periodically**, not on
every cookie change, so it lags a sign-in by seconds.

That file exists only while the browser is running. On a clean exit Gecko deletes the
recovery pair and writes the session to `sessionstore.jsonlz4` at the profile root
instead, so a scan that looks only for `recovery.jsonlz4` answers "no session found" for a
user who signed in and then quit — and the credential is a server-side session, still
good. Both names are searched, with every profile's live file offered ahead of any
profile's resting one, across all roots: the first candidate carrying `LS` wins, and a
resting file's credential can have expired while the browser that wrote it was closed.

Profiles are looked for under each browser's own root and under the sandboxed layouts too
— the snap that `apt install firefox` gives Ubuntu, and the Flatpak `~/.var/app` roots —
since a list that names only the unsandboxed path reports "no session" on the most common
Linux desktop there is.

Which profile the running installation is using is recorded in an `[Install<hash>]`
section, and Mozilla keeps that section in `profiles.ini` as well as in `installs.ini`.
Both are read, `profiles.ini` first. Reading only `installs.ini` loses the answer whenever
it is absent or stale — a profile tree moved to another machine, or restored from a backup
that took `profiles.ini` and the profile directories but not the undocumented sibling —
and ranking then falls back to the `Default=1` flag, which is the ordering this exists to
override. In `profiles.ini` the section name is what tells a profile path from a boolean:
`Default` means a path under `[Install<hash>]` and a flag under `[Profile<n>]`, so a reader
that ignores the section turns the flag into a profile directory named `1`.

The supported sources are therefore a session store, a pasted curl command, and a
`cookies.txt` export. Which one a path is gets decided by reading it: a session store is
identified by its `mozLz40\0` magic, a curl paste by its structure. Chromium keeps session
cookies in an encrypted SQLite database instead, so it is out of scope.

A paste is written for the shell it was copied on, and the reader has to undo that rather
than match one spelling. POSIX single quotes are literal; `$'…'` appears when a value holds
a quote; and on Windows the cmd form wraps every argument in `^"`, which escapes the quote
so cmd never counts itself as inside one and goes on stripping carets right through the
value — including the extra one in `%^7B` that stops a percent starting a variable
expansion. Reading only the quote style drops the Windows forms entirely, and reading the
value as written hands the store a credential with carets in it, which comes back as the
same opaque 500 a missing `LS` does. A value is never unescaped where the shell does not
escape, so a cookie that genuinely contains a caret survives as itself.

The flag carrying the jar varies too, and reading one spelling drops the credential from
the other exactly as reading one quote style does. A browser that writes the jar as a
header emits `-H 'cookie: …'`; one that uses curl's own cookie flag emits `-b '…'`, whose
value *is* the cookie string rather than a `Name: value` header. Both are read. `-b` also
accepts the name of a jar file to read instead of a cookie string, and curl tells the two
apart by whether the value holds an `=`, so this does too: taking a filename as the
credential would send the store a Cookie header whose whole content is a path.

The session store is read narrowly on purpose. It holds credentials for every host the
session touched, so `internal/session` filters to the `unity.com` family before anything
leaves the package, and no cookie value is ever logged.

Narrowing to a host is not quite narrowing to an account. Gecko records each cookie's
`originAttributes`, and Multi-Account Containers gives a container tab its own jar, so one
file can hold two `LS` cookies for the same host under two Unity accounts. Keyed on the name
alone they collapse to whichever the document happens to list last, and the run then
mirrors the other account's library — which `Resolved.Path` cannot diagnose, because both
came from the same file. The default context is taken first and a container fills only the
names it did not supply, so a container-only sign-in still resolves and a container tab
left open never silently outranks the session the rest of the browser is using.

The `_csrf` cookie is a double-submit token required by the GraphQL endpoint only. Not
every storefront route issues it — `/` and `/publishers/{id}` answer 200 and set nothing,
while `/packages` answers 404 and sets it. The bootstrap route is pinned to `/packages`,
treats its own 404 as normal, and is exempt from the redirect rule below.

It retries on the same terms every other call does. It is the first request a run makes,
so a 502 from the CDN in front of the store would otherwise end the run before any work
was done, while the identical fault one call later got a full backoff schedule — the
failure model has no clause for "except the first request". What does not retry is the
route answering *without* a token: that status is not one the retry rule accepts, and
proceeding from there guarantees a CSRF mismatch. The re-bootstrap that happens mid-run,
when a token expires between the bootstrap and the call using it, is still deliberately
worth exactly one more attempt and no schedule.

`x-requested-with: XMLHttpRequest` decides the *shape* of a failure: with it, a failed call
answers with the JSON error that carries the diagnosis; without it, the same call answers
302 to an HTML error page.

### Identity

Three ids come back per product. `id` is the store product id: the one
`/api/downloads/{id}` takes and the one stamped inside the package. `productId` is a
different 12-digit value no endpoint here accepts, and `itemId` is likewise unused. The
domain model carries only `id`.

Assets key on `slugify(name) + "-" + id`. That key is *not* the identity — a rename changes
it — so classification looks up prior entries by product id, and a rename re-keys the entry
and moves the cached directory rather than re-downloading.

### Two version identities

The store advertises `currentVersion.id`; the served package carries its own `version_id`.
These usually agree but sometimes do not, steadily: product 262163 advertises 1094273 and
serves 905463; 262495 advertises 1056339 and serves 839208. Twelve of fourteen measured
packages agree.

So the lockfile records both. `version.id` is the advertised value, refreshed every run.
`resolvedVersionId` is the advertised id the cached file was fetched against, and it is the
diff key. Diffing on the delivered id instead would make those products re-download
forever.

### The download endpoint

`GET /api/downloads/{id}` returns the bytes directly: 200, `application/octet-stream`, with
`Content-Disposition` usually but not always present. There is no CDN redirect, no
`Content-Length`, no `ETag`, and `Range` is ignored — so resume is impossible and nothing
pretends otherwise.

Two behaviours matter more than they look:

- An unauthenticated request answers **302 to Unity's OAuth authorize URL**. A client that
  follows redirects writes a sign-in page into the cache under a `.unitypackage` name.
- The endpoint honours `Accept-Encoding: gzip` by **gzipping the already-gzipped package**.
  Go does not transparently decode an encoding the caller asked for, so a client that sets
  the header itself caches a double-gzipped blob with no readable metadata. The tool sends
  `Accept-Encoding: identity` and treats a `Content-Encoding` on the response as an error —
  except `identity` itself, which asserts the body was *not* transformed. An intermediary
  that states the negotiated coding explicitly, a corporate proxy or a TLS-inspecting
  appliance, echoes it on a body that is the untouched package; refusing on non-emptiness
  alone failed every asset in the library behind one of those, and failed it unmarked, so
  each spent its full retry budget first and the diagnostic blamed the store for what the
  network in front of it did.

`downloadSize` is approximate: it runs 0-16 bytes above the bytes delivered, an artifact of
rounding up to a 16-byte boundary. It bounds a transfer; it never checksums one.

### Packages self-describe

A `.unitypackage` is gzip whose **FEXTRA** field carries a JSON descriptor in a subfield
with id `A$` — not the comment field, which is empty on every real package. It holds the
product id, the version id, the Unity version and the publisher. Reading it costs a header
parse, so a cached file can be identified without decompressing or hashing it.

The reader is driven by the subfield's own length. Every sampled package ends its
descriptor by byte 338, but XLEN is a uint16: a prefix-limited reader would silently report
"no metadata" for a package that has some, downgrading the hard wrong-asset check into a
tolerated warning.

## Run flow

```
1. Resolve config          user config dir, library path, session source
2. Load session            Cookie header; assert LS is present
3. Bootstrap CSRF          GET /packages (404, but sets _csrf)
4. Enumerate               page 0..n at pageSize 100; compare raw rows to page 0's `total`; dedup
5. Apply the allowlist     manifest entries with enabled = true, then --only
6. Sweep stale temps       walk the tree; before classification, so a partial is never adopted
                           cutoff is backdated past the stall window: the run start is
                           captured before enumeration, and a concurrent run's stalled
                           transfer is not junk until that run has given up on it too
7. Classify                Unchanged | New | Changed | DownloadNow | CacheMissing | Adopted | Undownloadable
8. Download the delta      bounded; guard against the temp file; commit; persist per asset
9. Finish                  final lockfile write and summary
```

`status` is steps 1-7 with `DryRun`, which also gates every mutating step: a dry run sweeps
nothing, moves nothing, and writes nothing.

Steps 6 and 7 are interruptible, which matters because they are where the time goes: under
`--verify` step 7 re-hashes the whole library, and the adopt path relocates and deletes
files before anything is persisted. The classification pass checks the context each asset
and breaks, the two whole-tree walks stop mid-walk, and the hash itself stops between reads
rather than at the end of the file — a single package reaches 23 GB, so per-asset
granularity would still be minutes. A cancelled run therefore reaches step 9 and records
what it did resolve. Without that the run keeps working — and keeps mutating — after the
interrupt, and a second Ctrl-C does nothing either, because the signal handler has already
taken SIGINT's default action away for the life of the run. The assets never reached are
counted as not attempted, so the exit status stays non-zero; an asset already in flight
when the pool was cancelled is counted with them rather than named as its own failure,
since the cause is the run's and repeating it per goroutine buries the one line that is
not.

## Where the guards live

The download stream crosses two packages, so ownership is fixed rather than left to
whoever writes the signature first:

- `store` owns the response-level guards, before any bytes are kept: no redirect followed,
  no `Content-Encoding` accepted, content type must be an octet-stream. It returns an open
  body plus the filename it parsed. The body it returns is wrapped in a stall guard, for
  the reason below.
- `cache` owns the write, in two phases. `Store` streams to a temp file beside the
  destination and hashes as it goes; `Commit` renames; `Discard` removes. Nothing
  unverified ever occupies a real cache path, because an interrupt in that window would
  strand a rejected body where the next run's adopt scan would take it for genuine.
- `syncer` owns the semantic guards, which need both the bytes and the enumeration
  metadata: the size floor and its re-query, then gzip magic and the descriptor's product
  id. The floor goes first, because the descriptor is the first ~350 bytes and a transfer
  that dropped inside them does not parse as gzip at all — a verdict the descriptor guard
  treats as a corrupt package and marks permanent, while the identical fault a few
  hundred bytes later reaches the floor and is retried. One transport failure must not
  have two outcomes decided by where the connection happened to drop.

Retry wraps store+cache together, so every attempt necessarily opens a fresh temp file and
a fresh hasher. Appending a retried response to a partial one would survive every other
check and then be hashed and recorded as its own truth.

## Stalls

A download carries no whole-request deadline, because a 23 GB package over a domestic link
legitimately takes hours and a deadline that kills it makes the mirror impossible on
exactly the connections that most need it. The response-header timeout bounds a server
that never answers. Neither bounds the case in between: headers arrive, then the body
stops without the connection closing, and `Read` blocks forever.

That one is the worst of the three. The read never returns, so the attempt never returns,
so `retry.Do` — which only inspects what its function returns — never gets to open a fresh
connection, and the pool slot is never given up. At the default concurrency of two, two
such transfers stop the whole run: no error, no progress line, no exit, and nothing to do
but interrupt it.

So the body is wrapped in a guard that resets a timer on every read returning bytes and
cancels the request when the timer expires. The window is generous (two minutes) because a
slow-but-live transfer must survive indefinitely; what it bounds is silence, not slowness.
The failure is named `ErrStalled` rather than left as the context cancellation underneath
it, which would read as an interrupt the user caused. It is retryable, because a fresh
connection is exactly what fixes it.

The guard rides on the body the download hands back, so it covers the accepted response and
nothing else — and a *rejected* one still has an open body. That body is drained rather than
abandoned, so the connection returns to the pool instead of being torn down, which means the
rejection path reads from a response this request carries no deadline for. A proxy or a
captive portal that answers 200 `text/html`, flushes, and then goes quiet lands exactly the
failure above, through the one door the guard is not on: a run that prints `fetching …` and
stops. The drain is therefore bounded by the API call's own deadline — a rejection body is
small, and a short one is the only kind that should ever arrive there. Cancelling before the
drain instead would sever the connection, which is the thing the drain exists to avoid.

The API calls are bounded end to end instead. They carry a few kilobytes of JSON, so a
body that stops arriving there is a server that will not finish rather than a slow link.

That window is also what the temp sweep has to spare, so the sweep's grace is derived from
it rather than chosen. Held as two independent numbers the grace was half of it, and a run
whose body went quiet at T had its temp unlinked by a second run starting at T+70s, then
resumed inside its own window and finished the copy into a file that no longer existed —
failing the whole transfer, permanently, because an opened-but-deleted temp reads as an
unparseable package rather than a truncated one.

## The size floor

Truncation is normally caught by the transport, but only for a *dropped* connection: a
stream the origin ends cleanly early yields a clean EOF, and with no `Content-Length`
nothing else notices. The descriptor lives in the first ~300 bytes, so it survives
truncation too.

So a received count below `downloadSize - min(4096, downloadSize/8)` fails that asset. The
allowance is absolute because the gap it forgives is a fixed alignment artifact, and
clamped because 4 KB is a third of the smallest owned package. A body outside the tight
±64 window but above the floor is a warning.

The floor is asked before the descriptor is read, so a short body is diagnosed as short
whatever it does or does not contain. It is the guard with something to say about a
truncated transfer, and the one whose verdict is retryable — which is what a truncation
is, since a fresh connection is what fixes it.

The one legitimate way to fall below the floor is a republish mid-download. The
discriminator is a single re-read of that product: if its advertised version or size moved,
it was a republish. It is deliberately *not* "the delivered id differs from the advertised
id", which is a steady state for some products and would switch the floor off permanently
for exactly them.

## Adoption

A package can be on disk with no lockfile resolution behind it: the lockfile was deleted,
the asset was mirrored on another machine, or a rename moved the path out from under the
record. Rather than re-fetching gigabytes, a run scans the library for a file whose own
descriptor claims that product and adopts it — recording its size, digest and delivered
version id, and moving it to the path the current layout dictates before recording it,
so quarry's facets and the lockfile agree.

The scan is one pass over the library per run, built on first use and shared by every
asset that asks. Probing per asset instead is quadratic exactly when adoption matters
most: a lost lockfile makes every owned asset ask, over a library that already holds them
all. Building it lazily matters too — the ordinary run, where everything is current and
nothing asks, must not pay for a walk. It is taken after the temp sweep, so an abandoned
partial is never in it.

Adoption is also what a delisted asset falls back on. A disabled product answers 404, so
it is the one class where a download can never make up the difference, and the lockfile is
not the only thing that knows the bytes are here — a deleted lockfile, or a mirror made on
another machine, leaves them on disk with nothing pointing at them. Reporting such an asset
as unavailable sends the user looking for a package already in their library and records
the entry untracked, so `list` stops counting it as mirrored.

An out-of-date asset asks too, with one restriction. `library_path` is user-scoped while
the manifest and lockfile are project-scoped, so several projects share one library and
keep separate lockfiles: project A syncs an asset to v2, and project B, whose lockfile
still records v1, has the new build already at the derived path. `Changed` was the one
class that never asked before fetching, so B re-transferred a byte-identical file — up to
23 GB, on a trigger as ordinary as a branch switch that reverts a lockfile. The probe it
gets accepts only a candidate already in place, because a copy elsewhere would have to
relocate and `Relocate` refuses an occupied destination: an asset whose derived path holds
a stale copy would start failing where today it downloads cleanly over it. It is also a
separate probe from the general one, which sets the damaged-file exclusion behind
`!cacheOK()` — under `--verify` that is a full re-hash of a file about to be replaced, and
it would exclude the very copy worth adopting.

Three gates keep adoption from laundering a bad file into the cache. The descriptor's
product id must match. Its version id must match what the store currently advertises, so a
stale build cannot be recorded as current. And the file must clear the same size floor a
download must clear, because that is the one route into the cache that skips the download
guards entirely.

All three are applied inside the scan's own selection rather than to the candidate it hands
back. The scan returns one file, preferring the copy already at the derived path, so gates
checked afterwards rejected that copy while another copy that would have passed sat
unexamined: a stale or truncated build sitting where the layout puts it masked an intact
one elsewhere in the library, and the asset re-downloaded in full. A cloud sync client's
conflicted copy is the ordinary way a library comes to hold two.

The second gate compares a *delivered* id against an *advertised* one, so for the products
where those differ as a steady state it can never pass: a perfectly good file for 262163 or
262495 is re-downloaded rather than adopted whenever the lockfile is missing. That is
accepted rather than overlooked. The alternative is trusting a delivered id no record
vouches for, and the case it would cover — no lockfile, no prior entry — is exactly the one
with no evidence to check it against.

A path the lockfile records is compared canonically, never as a string, and the whole
comparison runs in slash space so it answers the same on every platform. That file is
committed, hand-editable and read on other machines, so `./pub/a/a.unitypackage` has to be
recognised as the file `pub/a/a.unitypackage` names — and a backslash or a drive letter is
refused rather than interpreted, because `filepath.Clean` on Windows lifts a volume prefix
out before it resolves `..` and puts it back afterwards, so `Z:../../x` cleans to itself
and walks out of the root that a leading-`..` test would have caught anywhere else.
Treating two spellings as two files makes a run delete the copy it just downloaded as
though it were a superseded one, and leaves adoption unable to clear a damaged file off
the destination it needs.

Canonical spelling is not the whole answer on every platform. Windows and macOS as it is
usually configured are case-insensitive, so `Pub/a.unitypackage` and `pub/a.unitypackage`
are one file that no canonical form collapses. Every place that is about to delete or move
therefore asks the filesystem as well, through `os.SameFile`: the string comparison settles
it without touching the disk, and the filesystem settles what only it knows. The tool never
derives a path whose case varies — slugs are lowercased — so the spelling this catches
arrives from a hand-edit, a merge, or a case-normalising sync client over the library.

`Relocate` asks it too, and that one is not housekeeping. It refuses a destination that
already holds a file, because the caller records the digest of whatever ends up there — but
on those two platforms the occupant can be the source itself under its other spelling, and
refusing then fails the adopt of a package sitting exactly where it belongs. `Find` matches
that copy by identity and hands it over; without the same question at the move, the run
reports a failure, exits non-zero, and does it again on every later run.

Nor is a spelling the whole of confinement. `Canonical` refuses a path that leaves the root
lexically, and a path whose every segment is an ordinary name still leaves it when one of
those segments is a symlink, because a link is followed like any other directory. So every
operation that opens, creates, moves or deletes goes through `os.Root`, which resolves
inside the library at the syscall level and leaves no window between the check and the act.

The writes are in that set, and not only the reads. Held to `Canonical` alone they agreed
with the reads about spelling and disagreed about symlinks — and the permissive side was
the write, which is the worst way round. Symlinking a publisher directory onto a second
disk is a reasonable thing to do to a 75 GB library, and it produced a download that
succeeded, committed and recorded, followed by a `Verify` that refused the file just
written and an adopt scan that could not see it either, since `WalkDir` does not descend a
symlink. The asset classified `CacheMissing` and re-downloaded in full on the next run, and
on every run after it, reported only as `cache-missing 1`. Both gates now refuse the same
paths, so the asset fails once with an error naming the segment that is a link. `os.Root`
has no `CreateTemp`, so the cache does that job through it; the library root itself is
still created outside the root, because a first run has none to open.

The two whole-tree walks go through the root as well, and for a reason that has nothing to
do with escaping it: `filepath.WalkDir` opens with an `Lstat` and stops at anything that is
not a directory, so a `library_path` that is itself a symlink — the same move, one level
up, and what a Windows junction reports as — made both of them visit the link and descend
nothing. Everything else kept working, since `os.Root` resolves a symlinked root like any
other directory, which is what made it silent: the adopt scan returned an empty index, so
every owned asset classified `cache-missing` and re-downloaded in full on every run and a
delisted asset already on disk read as unavailable, while the temp sweep reclaimed nothing
and said so only by reporting a count the summary omits when it is zero. Walking the root's
own FS stats `.` through it instead, so the case cannot arise. A symlinked directory
*inside* the library is still not descended, which is the half the confinement rule rests
on, and a package that is itself a link out of the tree is now refused at the open rather
than followed.

A file that just failed verification is excluded from the scan. A truncation or a mid-file
flip leaves the descriptor intact and a small truncation clears the floor, so without that
exclusion the damaged bytes would be re-hashed and their digest recorded as the asset's
truth — the precise outcome every other guard exists to prevent.

A run deletes a package only when it is replacing that same asset's own copy, which
happens three ways: a download lands at a different derived path than the entry's previous
one, an adoption does the same, or an adoption replaces a recorded copy that failed its
check and is sitting on the destination. None of these is the de-owned case, which is
reported and left in place. The cache holds only current versions, so a superseded copy of
the same asset is not something to keep, but a copy the tool did not write is never
touched: every path removed here came out of the lockfile.

That the path came out of the lockfile used to be the whole of the argument, and it is not
enough. The file is committed, hand-editable and merged across machines, and nothing
refused two entries naming one path — so with asset A's `cachePath` pointing at asset B's
package, B verified against its own entry and carried forward `Unchanged`, A's verify
failed on size and downloaded, and the superseded-copy cleanup unlinked B. The run exits 0,
the lockfile claims B is mirrored with a digest at a path holding nothing, and B
re-downloads in full on every later run. Tightening `Verify` does not reach it, since a
false `Verify` is exactly what routes A into the delete. So the removal asks the bytes what
they are: the file's own descriptor has to name the asset being replaced. A package
carrying no descriptor is still removed, because some genuinely have none and refusing
those would make them undeletable. The duplicate is also refused at the start of a run,
before anything has been swept, moved or deleted.

## Failure model

| Observation | Meaning |
| --- | --- |
| 400 `csrf token mismatch` | bootstrap failed |
| 500 + empty `GraphqlError` | session expired; not retried, because the status alone would say to retry |
| 3xx from any store endpoint | session expired; never followed |
| 404 on a download | the asset was pulled; permanent, so it does not fail the run |
| 429 / 408 / 5xx elsewhere | retried with backoff |
| rows collected != `total` | loud error, never a silent short walk |
| rows collected > `total` | loud error; the walk ends on an empty page, and a store that clamped an over-range page would otherwise loop forever |
| 200 with a non-empty `errors` array | loud error, never "you own nothing" |
| a body that stops mid-transfer | `ErrStalled` after the silence window; retried, because a fresh connection is the fix |

A failed download fails its asset, not the run: one delisted or corrupt package must not
stop a 75 GB mirror. The pool cancels early only for a run-fatal error. The exit status
separates actionable from permanent — a corrupt body exits non-zero, a pulled asset does
not.

The assets a cancelled pool never reached are counted apart from the ones that failed.
Nothing is known about them, so the summary gives them one line rather than a line each:
a session dying at asset 5 of 300 would otherwise bury the one message the user can act on
under 295 identical cancellations. They still keep the exit status non-zero, because the
run did not do what it was asked.

An asset whose bytes are in place and whose lockfile entry could not be written is counted
apart from both, and also keeps the exit status non-zero. All three places that persist —
the download, the adoption, the relocation a rename forces — report it the same way, as a
warning naming the asset rather than as a failure of it: the work was done, and it is the
record of the work that was lost. The exit status is the half that matters, because the
lockfile is what the next run reads. A relocation it does not know about classifies
`CacheMissing` and re-downloads the package in full, on every run, so a run that reported
this as a warning alone exited 0 having made the library unreadable to its successor.

## The select page

`select` serves the owned-asset list on loopback and takes one save back. Three things
stand between that page and a curated manifest, and none of them is advisory.

The page is served only to a browser on this machine, decided by two things together. The
bind address is refused unless it names one address: a wildcard binds every interface, and
`Host` is written by the client, so a request off the network claiming `localhost` is
indistinguishable from a browser here. Naming a non-loopback address stays allowed, because
that is a deliberate exposure rather than a reach for a port number.

Given an address, each request's `Host` is checked against it — and the check refuses an
unspecified one itself rather than relying on the bind being validated first. The two
tests it would otherwise reach, a `Host` of `localhost` and a loopback literal, are true
of a request from anywhere on the network when there is no bound address to compare them
against, so held only at the bind the control was one caller away from switching off with
nothing in the page's own guards failing. The per-run token in the
form stops a blind cross-origin POST, since a page on another origin cannot read it out of
this one — but it does nothing against DNS rebinding, where a page the user is already on
re-resolves its own name to a loopback address and the browser then treats it as
same-origin *by name*. Its script could read the rendered list, which is the account's
purchase history, and the token with it. So the check runs before the render, not only
before the save.

The save is accepted exactly once, through a `sync.Once` rather than by assumption: two
tabs carry the same per-run token, so without it the second POST is answered "Saved …" for
a selection nothing is still reading, telling that user their choice was kept while the
manifest holds the other tab's.

The manifest refuses two entries for one asset at load, for the reason the lockfile does
and by the same route: a merge that kept both sides of one. Accepting a duplicate makes
the two readers disagree — the enabled set takes `true` from whichever block has it, so
`sync` mirrors the asset, while the reconcile keeps the last block in file order, so the
page renders it unchecked. Saving from that page collapses the pair to disabled and the
asset silently stops being mirrored.

A save that would deselect everything is refused rather than written, unless nothing was
selected to begin with. The comparison is against the set `Reconcile` has already
rewritten, so a de-owned asset dropping out is not mistaken for the user clearing the list.

## Lockfile

Committed beside the manifest as `unity-sync.lock.json`, keyed by asset slug, with
`assetId` inside each entry. Every owned asset gets an entry, whether or not it is
selected, because the file records what is *owned*.

Each entry has two halves. The advertised half (`name`, `state`, `publisher`, `version`,
`advertisedSize`) refreshes every run. The resolution half (`tracked`, `resolvedVersionId`,
`deliveredVersionId`, `sizeBytes`, `sha256`, `cachePath`, `downloadedAt`, `storeFilename`)
is rewritten only when the run resolves that asset, and is otherwise carried forward
verbatim — along with the entry's key, so key and path cannot drift apart.

`sizeBytes` is always the received count, never the advertised one. There is no run
timestamp: stamping one would dirty a committed file on every no-op run.

It is rewritten as each asset resolves, not once at the end, and that applies to the
classification pass as much as to the downloads. Both move and delete packages before
anything is recorded, so a single closing write is a single point of loss: a run that
adopts and fetches nothing rides entirely on it, and losing it to a held-open file, a full
disk or a kill leaves the library moved while the lockfile still names the old path with a
digest that no longer describes the bytes there. The next run verifies that entry against
the file now sitting at the recorded path, finds the size and version it expects, calls it
`Unchanged` and carries the stale digest forward. Nothing but `--verify` looks again.

It is published the way every other file here is — write a temp beside the destination,
flush it, rename — and the flush is the load-bearing half of that: a rename is durable
ahead of the data it publishes, so bytes still in the page cache when it returns are
exactly the per-asset progress this write exists to keep. Every error path unlinks its own
temp, but a kill or a power loss between the create and the rename cannot, and this file is
written once per resolved asset, so a large sync spends a lot of windows there. The
leftovers land in the directory the user commits and nothing else would ever remove them,
so the run sweeps them alongside the cache's, under the same backdated cutoff that spares a
concurrent run's write in flight.

Those eight fields are one type rather than a convention, embedded in the entry, so the
branch that carries a resolution forward and the branch that writes a fresh one cannot
drift: a ninth field added to only one of them would be silently dropped from every entry a
run does not resolve, which on a no-op run is the whole file.

Two entries for one product are refused at load. A run never writes such a file, since the
key is derived from the id — but a rename changes an entry's key by construction, so a
merge that keeps both sides of one leaves a duplicate, and entries are looked up by walking
the map. The run would then pick between them at random: the same checkout classifies the
asset `Unchanged` on one run and `Changed` on the next, re-fetching gigabytes on a coin
flip, and the entry not picked is dropped without a word.

## Cache layout

```
<library>/<publisher-slug>/<asset-slug>/<asset-slug>.unitypackage
```

Three segments because quarry derives its vendor facet from the first path segment and its
pack facet from the second, filling the latter only when a path has at least three parts. A
flat tree would index every package with both facets empty.

The filename is derived, not taken from `Content-Disposition`, which the store sends
inconsistently — trusting it would let one asset land under two names across runs and put a
machine-dependent path into a committed lockfile.

Both slugs fall back when a name folds to nothing under ASCII folding: the publisher
segment becomes `publisher-<id>` and the asset segment the bare product id. An empty
segment would collapse the layout and empty quarry's facets.

The publisher segment falls back for a second reason. It carries no id suffix, so unlike
the asset segment it can come out as a bare word — and a publisher whose name folds to one
of the names Windows reserves for a device (`con`, `aux`, `com1` and the rest) would derive
a directory `MkdirAll` cannot create there and nowhere else. Since a failed download fails
only its asset, that reads as a run which quietly never completes. `cache.safeSegment`
refuses such a segment arriving by any other route.

## Testing

The default suite is fully offline: `httptest` servers plus committed fixtures scrubbed
from real captures. It runs on Linux, Windows and macOS, because several checks select on
the host and are otherwise half dead code: both signature tables — the Go one and
`install.sh`'s — resolve to the running platform, so on Linux alone only the ELF arm is
ever exercised, and the Gecko profile roots and the rename that replaces a running
executable are per-platform for the same reason. `install.sh` is covered too, by running
the real script against a stub release: it composes the asset label from `uname` in its own
language, and the guard holds that composition to the labels `release.yml` actually
publishes, which is otherwise the one contract nothing compiles together. The fixtures
carry no account data, and a guard test fails the build if any appears. Raw captures are
never committed, and the scrubber lands before anything that consumes fixtures, because git
keeps what a later commit deletes.

`go run ./cmd/scrubfixtures` regenerates `testdata/store` from a `captures/` directory of
raw `SearchMyAssets` responses, one JSON per page. That directory is git-ignored and is not
in the repo: the captures need a signed-in session, so regenerating the fixtures means
capturing again rather than re-running the scrubber over something checked in.

The committed fixtures are held to being that tool's output, by re-scrubbing each one and
comparing bytes. The forbidden-field walk names five fields; the scrub's allowlist is what
actually decides, so a hand-added field the pinned query never asked for matched nothing
and was committed, public and permanent with the suite green — and then silently reverted
by the next legitimate regeneration. Re-scrubbing settles it, because the scrub is
idempotent on its own output.

No compiled binary is tracked, which CI checks by asking `file` about the whole tracked
set. Nothing else here would ever notice: `go build ./...` discards executables when given
more than one package, `go vet` and `gofmt` enumerate `.go` files, and the fixture guard
opens only paths under a `testdata/` directory. A 6.8 MB artifact left at the root by
`go build ./cmd/scrubfixtures` was committed and stayed in history.

That check did not work for its whole life. Given more than one file, `file` pads the
names out to the longest one, so a `': application/…'` pattern matched only whichever
tracked path happened to be longest and nothing else — the guard passed over every binary
it was written to catch, including the one above. It matches any run of whitespace now,
and it ends by asking `file` about two probe binaries whose names differ in length and
requiring *both* to match: the longest name is the single line that still has exactly one
space after its colon, so a one-space pattern matches it and passes, which is precisely
how this sat green while seeing nothing. The pin check alongside it grew the same kind of
self-check, since both ended in `|| true` and an is-it-empty test, making "found nothing
wrong" and "found nothing at all" the same result — the one standard every other guard in
this repo already meets.

## Distribution

An update that is going to refuse itself reads no credential at all: the dev-build refusal
comes before the lookup, or `unity-sync update` on a dev build spawns `gh auth token` to
build a client it throws away — and the test suite drives that path, so it did the same on
every run.

`install.sh` and `unity-sync update` both read the public release API, so neither needs a
GitHub credential; one is used when the environment or `gh` supplies it, and buys only the
authenticated rate limit. The installer passes it through a curl config on stdin rather
than as an argument, which every local user can read out of `ps`. The updater checks that
the asset URL it found in the release JSON is on the API host before requesting it: Go
strips the Authorization header on a redirect to another host, which covers the hop to the
signed CDN, but nothing covers the first request. The archive and the binary inside it are
both read under a ceiling, so an artifact that is not one of the published zips is an
error naming the size rather than an update the kernel kills.

Neither reader attaches it to an API base it was not built with. The updater hardcodes the
host; the installer takes one from the environment as a test seam, and refuses to send a
credential anywhere else. That seam is otherwise a way to turn "can set an environment
variable" — a shared container image, a CI job definition — into "has this user's GitHub
token", which `gh auth token` would hand over out of a keyring the env-setter cannot read
for themselves.

Because the credential is only an optimisation, one the API rejects must not be worse than
none: both readers retry anonymously on a 401, a 403 or a 404. That marker decides whether
to retry and never what to report, and the installer needs the status for that as much as
the updater does — `curl -f` fails alike on every status at or above 400 and on every
transport failure, so a shell fetch that keyed on its exit status blamed the credential for
the API answering 502, for a proxy reset, for no DNS. Only one of those three statuses ever
means "this credential", so `unity-sync update 0.2.9` for a version that was never tagged
answers 404 and must say so — reporting it as a rejected credential sends a user who has none looking
for one, with the real cause buried behind a claim about a thing they do not have. When the
anonymous attempt fails too, the credential is demonstrably not what is in the way, so what
that attempt said is what gets reported. An expired token left in `GITHUB_TOKEN` would
otherwise kill the documented upgrade path with "Bad credentials", and a fine-grained one
never granted public-repository read answers 404 — which reads as "no release exists"
rather than as anything to do with the token. For the same reason both ask `gh` for
`github.com` explicitly: `gh auth token` otherwise answers for whichever host is logged in,
so a user authenticated only against their company's GitHub Enterprise would have that
token sent to `api.github.com`.

Every third-party action is pinned by commit, and CI refuses a tag. The check walks the
whole of `.github/`, not just `workflows/`: steps factored into a local composite action
are referenced as `uses: ./.github/actions/<name>`, which the check ignores by design — but
that action's own `uses:` lines run in the same job, beside `contents: write` and
`id-token: write`, and a walk that stopped at `workflows/` would never see them.

The release attests build provenance, because `update` replaces the binary on PATH
unattended and TLS to GitHub was otherwise the only thing vouching for the bytes. The
attestation is a signed statement that the release workflow built that artifact from that
commit, verifiable with `gh attestation verify <file> --repo curbol/unity-sync`. Publishing
a checksum the updater did not check would have been worse than publishing nothing.

Both publish the same way the lockfile is written — flush, then rename — because a rename
is durable ahead of the data it publishes, and a crash inside the writeback window would
otherwise leave a truncated binary on PATH.

The asset body carries a stall guard, for the reason the download path does. The
response-header timeout bounds a server that never answers and there is deliberately no
whole-request deadline, and neither covers the case in between: headers arrive, the body
stops without the connection closing, and the read parks forever — `unity-sync update`
sitting with no output and nothing to do but interrupt it. The window is its own number
rather than the store's and deliberately shorter, since that one is sized for a 23 GB
package and the published zips are single-digit megabytes; nothing couples the two, unlike
the temp sweep's grace.

Neither the installer nor the updater installs bytes it has not recognised as a native
binary. The zip reader already verifies each entry's CRC, so what the magic-byte check
catches is the other way this goes wrong: a release that shipped an error page, a script,
or an empty file under the right asset name. It runs before the rename in both, because
after it the working binary is gone — the installer's closing smoke test notices a broken
install, but only once there is nothing left to fall back to.

Replacing a running binary is a rename on every platform but Windows, which refuses to
replace a running image but does allow it to be renamed. So the update moves the old binary
to `<target>.old`, takes its name, and puts it back if that second rename fails. Leaving
nothing on PATH is the one outcome an updater must never produce, and when even the restore
fails the error names where the working binary went.

## Open questions

- Whether the Unity Editor recognises this cache layout if `library_path` points at its
  `Asset Store-5.x` directory. Untested; the docs claim nothing.
