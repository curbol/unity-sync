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
| Package bytes | `GET /api/downloads/{productId}` |

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

The supported sources are therefore a session store, a pasted curl command, and a
`cookies.txt` export. Which one a path is gets decided by reading it: a session store is
identified by its `mozLz40\0` magic, a curl paste by its structure. Chromium keeps session
cookies in an encrypted SQLite database instead, so it is out of scope.

The session store is read narrowly on purpose. It holds credentials for every host the
session touched, so `internal/session` filters to the `unity.com` family before anything
leaves the package, and no cookie value is ever logged.

The `_csrf` cookie is a double-submit token required by the GraphQL endpoint only. Not
every storefront route issues it — `/` and `/publishers/{id}` answer 200 and set nothing,
while `/packages` answers 404 and sets it. The bootstrap route is pinned to `/packages`,
treats its own 404 as normal, and is exempt from the redirect rule below.

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
  `Accept-Encoding: identity` and treats any `Content-Encoding` on the response as an error.

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
4. Enumerate               page 0..n at pageSize 100; compare raw rows to `total`; dedup
5. Apply the allowlist     manifest entries with enabled = true, then --only
6. Sweep stale temps       walk the tree; before classification, so a partial is never adopted
7. Classify                Unchanged | New | Changed | DownloadNow | CacheMissing | Adopted | Undownloadable
8. Download the delta      bounded; guard against the temp file; commit; persist per asset
9. Finish                  final lockfile write and summary
```

`status` is steps 1-7 with `DryRun`, which also gates every mutating step: a dry run sweeps
nothing, moves nothing, and writes nothing.

## Where the guards live

The download stream crosses two packages, so ownership is fixed rather than left to
whoever writes the signature first:

- `store` owns the response-level guards, before any bytes are kept: no redirect followed,
  no `Content-Encoding` accepted, content type must be an octet-stream. It returns an open
  body plus the filename it parsed.
- `cache` owns the write, in two phases. `Store` streams to a temp file beside the
  destination and hashes as it goes; `Commit` renames; `Discard` removes. Nothing
  unverified ever occupies a real cache path, because an interrupt in that window would
  strand a rejected body where the next run's adopt scan would take it for genuine.
- `syncer` owns the semantic guards, which need both the bytes and the enumeration
  metadata: gzip magic, the descriptor's product id, the size floor and its re-query.

Retry wraps store+cache together, so every attempt necessarily opens a fresh temp file and
a fresh hasher. Appending a retried response to a partial one would survive every other
check and then be hashed and recorded as its own truth.

## The size floor

Truncation is normally caught by the transport, but only for a *dropped* connection: a
stream the origin ends cleanly early yields a clean EOF, and with no `Content-Length`
nothing else notices. The descriptor lives in the first ~300 bytes, so it survives
truncation too.

So a received count below `downloadSize - min(4096, downloadSize/8)` fails that asset. The
allowance is absolute because the gap it forgives is a fixed alignment artifact, and
clamped because 4 KB is a third of the smallest owned package. A body outside the tight
±64 window but above the floor is a warning.

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

Three gates keep adoption from laundering a bad file into the cache. The descriptor's
product id must match. Its version id must match what the store currently advertises, so a
stale build cannot be recorded as current. And the file must clear the same size floor a
download must clear, because that is the one route into the cache that skips the download
guards entirely.

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

## Failure model

| Observation | Meaning |
| --- | --- |
| 400 `csrf token mismatch` | bootstrap failed |
| 500 + empty `GraphqlError` | session expired; not retried, because the status alone would say to retry |
| 3xx from any store endpoint | session expired; never followed |
| 404 on a download | the asset was pulled; permanent, so it does not fail the run |
| 429 / 408 / 5xx elsewhere | retried with backoff |
| rows collected != `total` | loud error, never a silent short walk |
| 200 with a non-empty `errors` array | loud error, never "you own nothing" |

A failed download fails its asset, not the run: one delisted or corrupt package must not
stop a 75 GB mirror. The pool cancels early only for a run-fatal error. The exit status
separates actionable from permanent — a corrupt body exits non-zero, a pulled asset does
not.

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

## Testing

The default suite is fully offline: `httptest` servers plus committed fixtures scrubbed
from real captures. The fixtures carry no account data, and a guard test fails the build if
any appears. Raw captures are never committed, and the scrubber lands before anything that
consumes fixtures, because git keeps what a later commit deletes.

`go run ./cmd/scrubfixtures` regenerates `testdata/store` from a `captures/` directory of
raw `SearchMyAssets` responses, one JSON per page. That directory is git-ignored and is not
in the repo: the captures need a signed-in session, so regenerating the fixtures means
capturing again rather than re-running the scrubber over something checked in.

## Open questions

- Whether the Unity Editor recognises this cache layout if `library_path` points at its
  `Asset Store-5.x` directory. Untested; the docs claim nothing.
