# unity-sync

A Go CLI that mirrors the assets you own on the Unity Asset Store into a local library,
downloading only what changed since the last run. It is the download manager the Asset
Store does not give you outside the Editor. Design: `docs/design.md`.

## Install

Grab the latest release into `~/.local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/curbol/unity-sync/main/install.sh | bash
```

No GitHub credential is needed. A `GITHUB_TOKEN`, `GH_TOKEN` or `gh` login is used when
present, only to get the authenticated API rate limit.

Then update in place:

```bash
unity-sync update           # latest release
unity-sync update 0.2.0     # a specific version
unity-sync version          # what is installed
```

Releases are cut by pushing a `v*` tag; a workflow builds the cross-platform binaries and
publishes them.

## Build from source

```bash
go build -o unity-sync .
```

Requires Go 1.26+. No cgo. To stamp a version into a local build:
`go build -ldflags "-X main.version=0.2.0" -o unity-sync .`

## One-time setup

Two kinds of state, kept apart:

- **User config** (session, machine defaults) lives *outside* any project, resolved as
  `--config <dir>` › `$UNITY_SYNC_CONFIG_DIR` › `$XDG_CONFIG_HOME/unity-sync` ›
  `~/.config/unity-sync`.

  ```bash
  mkdir -p ~/.config/unity-sync
  cp config.example.toml ~/.config/unity-sync/config.toml
  ```

- **Project manifest** (`unity-sync.toml`: which assets this project draws from) lives
  *in* the project that consumes the assets, committed to its repo. unity-sync finds it by
  walking up from the working directory, or point at it with `--manifest`. Its lockfile
  (`unity-sync.lock.json`) is written beside it. It carries no account identity. See
  `unity-sync.example.toml`.

Packages are cached at `$XDG_DATA_HOME/unity-sync` (`~/.local/share/unity-sync`) by
default; override with `library_path`, `UNITY_SYNC_LIBRARY`, or `--library`.

## Session

The store gates everything behind your signed-in session, and the cookie it actually
checks is `LS`. It is a session cookie, so `cookies.sqlite` never holds it — but a
Firefox-family session store does, because Gecko records the cookies of every host the
browsing session touched.

**If you use Firefox, Zen, LibreWolf, Waterfox or Floorp**, sign in to the Asset Store once
in that browser and point unity-sync at it:

```toml
# ~/.config/unity-sync/config.toml
session_source = "browser"
```

```bash
unity-sync status     # no paste, no expiry to babysit
```

It reads only the `unity.com` cookies out of the session store and discards the rest of the
file. You do not need to keep an Asset Store tab open; the cookie lasts as long as the
browsing session does. `--session browser` does the same thing for one run, and the run
prints which profile it read.

Chromium-family browsers keep session cookies somewhere else entirely, encrypted, so they
are not supported.

**Otherwise, paste a session.** In DevTools → Network, right-click any
`assetstore.unity.com` request → Copy → Copy as cURL, and save the whole thing verbatim —
no extracting values, no escaping:

```bash
$EDITOR ~/.config/unity-sync/session.curl     # paste, save
unity-sync status
```

A Netscape `cookies.txt` export works too, as long as your exporter keeps HttpOnly rows
(they are written with a `#HttpOnly_` prefix). Point at either file with `--session`, with
`session_source` in `config.toml`, or just save it as `session.curl` or `cookies.txt` in
the config dir, where unity-sync looks by default.

`--session` also takes a browser profile directory or a `recovery.jsonlz4` straight, which
covers a Gecko browser this does not know where to look for. Whatever you point it at,
unity-sync works out what the file is by reading it.

If the session has no `LS` cookie, unity-sync says so before making any request, because
the store's own answer in that case is an HTTP 500 that reads like a server fault.

A pasted session expires. When it does, re-copy it, or switch to `session_source =
"browser"` and stop re-copying.

`UNITY_SYNC_SESSION` sets the same thing from the environment, between `config.toml` and
`--session` in precedence.

## Commands

```bash
unity-sync select   # pick which assets to mirror (opens a local page)
unity-sync status   # what a sync would change; downloads nothing, changes nothing
unity-sync sync     # download the delta and update the lockfile
unity-sync list     # print the current lockfile
```

Useful flags: `--manifest <path>`, `--only <asset-slug-glob>`, `--library <dir>`,
`--concurrency <n>`, `--verify`, `--dry-run` (makes `sync` behave like `status`),
`--config <dir>`, `--session <file>`, `--addr <host:port>` (the `select` page's address;
it must name an address on this machine, since the page lists everything the account owns).

## Selecting assets

Selection is opt-in: an asset is mirrored only once you enable it. This matters more here
than it might sound — a typical Asset Store account owns hundreds of packages and tens of
gigabytes, and individual packages reach 23 GB.

`unity-sync select` lists every owned asset with its thumbnail and a checkbox and writes
the `[[asset]]` entries into `unity-sync.toml`:

```toml
[[asset]]
  id = "115488"
  name = "Quick Outline"
  enabled = true
```

Entries key on the asset id, so a publisher renaming their asset cannot silently deselect
it. Newly-bought assets appear disabled on the next `select`, so buying something never
downloads it behind your back. Hand-editing the file is fine.

`select` is the only command that writes the manifest. `status` and `sync` only read it.

## What it does

- Lists every owned asset with its current version inline, so a run that changes nothing
  costs only the enumeration: one bootstrap, a page request per 100 owned assets plus the
  empty page that ends the walk, and no package bytes at all.
- Downloads only what is new, changed, or missing from the cache, into
  `<library>/<publisher>/<asset>/<asset>.unitypackage`.
- Records everything in `unity-sync.lock.json` beside the manifest: what is owned, at what
  version, what is mirrored, and its checksum. Commit it for a changelog.
- Reports assets your manifest lists that the account does not own, and assets the store
  has delisted.

## Verifying the cache

Every run checks cached files cheaply: the file exists, its size is exactly what was
recorded, and — for packages that carry a version stamp — that stamp still matches. That
catches a truncated or replaced file without reading tens of gigabytes. A package with no
stamp is checked on size alone, so it does not re-download on every run.

`--verify` re-hashes instead, which is the only way to catch corruption in the middle of a
file. It is opt-in for the obvious reason.

## Cache

The library is local and expendable: current versions are re-downloadable, and `sync`
re-fetches anything missing or failing its check. Deleting the cache and re-syncing
rebuilds it. Durability of the assets you actually ship belongs in the consuming project,
not here.

When an asset leaves your account, unity-sync does not delete anything: its entry drops
out of the lockfile and the run tells you which file is now unreferenced, so you can
decide. A run removes a file only when it is replacing that same asset's own copy: the
superseded build after its path changed, whether the new copy was downloaded or adopted,
and a copy that failed its check when a good copy of the same package is adopted over it.

## Browsing what you have

Searching and previewing the mirrored library lives in a separate tool,
[quarry](https://github.com/curbol/quarry): it indexes an asset tree, reads inside
archives, and previews models. The cache layout here is three levels deep
(`publisher/asset/file`) precisely so quarry's vendor and pack filters work against it.
Point its `root` at your library path.

## Notes

- Test fixtures are generated: `cmd/scrubfixtures` regenerates the PII-free
  `testdata/store/` from git-excluded raw captures, and a guard test fails the build if
  account data reaches them.
- The tool is polite: bounded concurrent downloads (2 by default), backoff on rate limits,
  and it identifies itself with a User-Agent.

## Support

<a href="https://ko-fi.com/curbol"><img height="42" alt="Buy Me a Coffee at ko-fi.com" src="https://storage.ko-fi.com/cdn/kofi1.png?v=6"></a>
