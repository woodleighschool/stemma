# Sources and updates

`MacSoftware` and `WindowsSoftware` each have one `source`. `BuildMacPkg` has named
`inputs`. Both use the same resolvers and lockfile. Windows accompanying files use
these resolvers too.

## Download a file

```yaml
source:
  url: https://dl.google.com/dl/chrome/install/googlechromestandaloneenterprise64.msi
```

The filename comes from `Content-Disposition`, then the final URL's basename, then
the original URL's basename. It is pinned in the lockfile. Set `filename` when a
vendor endpoint returns an unusable name:

```yaml
source:
  url: https://vendor.example/download
  filename: VendorSetup.exe
  headers:
    User-Agent: VendorDeploymentClient
  token: "{{ env.VENDOR_DOWNLOAD_TOKEN }}"
```

`token` supplies a bearer token. It cannot be combined with an `Authorization`
header. Credentials and custom headers stay confined to the source origin across
redirects. An optional `sha256` asserts a known digest in the declaration.
Pair it with `filename` to resolve without downloading. Otherwise the first
fetch discovers the filename, which later updates can reuse with that digest.

The lock keeps only a digest of this declaration, so a URL that works as a
credential can come from `{{ env.NAME }}`. Set `filename` with it; the default
name can come from the URL.

## Discover a release

GitHub release assets use the `github` resolver:

```yaml
source:
  resolver: github
  repository: company/application
  release: latest
  asset: Application-*-arm64.zip
```

Replace the repository and asset pattern with the publisher's values. `asset` is
an asset-name glob using the same doublestar syntax as local input patterns, not
a regular expression. Exact names work too. The pattern must match exactly one
asset in the selected release; zero or multiple matches fail. For example,
`SafeExamBrowser-*.dmg` selects `SafeExamBrowser-3.7.1.dmg` without declaring a version.

`release: latest` and an omitted or empty `release` select GitHub's latest release.
A value containing `*`, `?` or `[` is a tag glob with the same syntax as `asset`:
discovery selects the newest published release whose tag matches, so
`release: v3.*` follows one release line. Any other value selects that exact
release tag. Draft releases are rejected.

For GitHub, `inputs.<name>.version` is the raw release tag, such as `v1.2.3` or
`Audacity-3.7.3`. Select or transform it explicitly when a package requires a
numeric version.

Set `include_prereleases: true` to include prereleases: `latest` selects the newest
published release across both stable releases and prereleases, and a tag glob also
matches prereleases. Tag globs and prerelease discovery compare `published_at`
across all pages, skip drafts, and use the higher release ID to break equal
timestamps. An explicit `release` tag still selects that exact release. Asset
matching applies to the selected release; a missing asset does not fall back to an
older release. The lock continues to pin the concrete release and asset identities.

```yaml
source:
  resolver: github
  repository: bambulab/BambuStudio
  asset: Bambu_Studio_mac_*.dmg
  include_prereleases: true
```

For an HTTP page containing a download link, use `resolver: http` with `url` and a
`match` regular expression. HTML pages match on element attribute values, so links
in comments, page text and script content are ignored; a plain-text release feed
matches on the response body. The full match can be an absolute, protocol-relative, root-relative or path-relative
URL. References resolve against the page's base URL: the final URL after
redirects, or the `base` element when the page declares one. Equivalent references count as one URL. All matches must resolve to one distinct
stable HTTP(S) URL. The lock stores that absolute URL, so locked fetches do not
repeat discovery or reapply the pattern. Credentials remain confined to the
configured source origin. Discovery belongs to the resolver; the software kind still receives
one file. External plugins can supply other [resolvers](writing-plugins.md#resolvers).

## Follow Homebrew releases

Homebrew casks and bottles provide download hashes, so `stemma update` checks for
new releases without downloading installers, even with an empty cache. `prepare`
downloads the selected file when needed and verifies its hash.

```yaml
source:
  resolver: homebrew
  cask: vlc
```

A cask selects the vendor download. `architecture` defaults to `arm64` and
`macos` to the current supported release, macOS 27. Both describe the target;
runner architecture and OS do not participate. Set `architecture: x86_64` or a
supported macOS major version explicitly when needed. `language` selects a cask
language alias; omission uses its declared default.

Use `formula: mole` instead of `cask` to select a Homebrew core bottle. Formula
sources require `any_skip_relocation`, no runtime formula dependencies and no
post-install actions. An older bottle of the same architecture can satisfy a
newer target. There is no source-build fallback or Homebrew prefix emulation.

A cask may supply a universal or Intel artifact for an Apple silicon target.
Hashless and `no_check` casks still check their vendor bytes during update.

A bottle remains a source archive. Use `BuildMacPkg` to declare the installed
layout, including support files and command links, then publish that package
with `MacSoftware`. `MacSoftware` requires an application or installer; it does
not choose an installation location for a command-line tool.

In a build, `inputs.<name>.version` includes the upstream version, formula
revision and bottle rebuild, for example `1.56.1.0.0`. `$input` selects the
formula's content root, so `path: libexec` works without knowing the bottle's
archive layout. Omitting `path` copies that whole root. Detailed Homebrew claims
remain available in `inputs.<name>.evidence`.

## Follow WinGet releases

WinGet also supplies installer hashes: checking for updates does not require
downloading the installer.

```yaml
source:
  resolver: winget
  package: Google.Chrome
  architecture: x64
  scope: machine
  installer_type: wix
```

The community source supplies the latest version by default. `version` can select
an exact published version. Version selection happens before installer filtering;
a missing match never falls back to an older release. Architecture defaults to
`x64` independently of the runner. `scope`, `installer_type` and `locale` filter
explicit manifest claims. Exactly one installer must match. An absent installer
locale remains absent; the package description's locale does not fill it.

The lock retains the selected installer and its declared settings under
`winget.installer` evidence. Stemma does not run the WinGet client or install its
dependencies. Store-only, authenticated and download-prohibited installers are
unsupported.

## Use local files

```yaml
source:
  path: Assets/Vendor.pkg
```

`path` can select a file or directory. Relative paths resolve from the resource
file, not the shell's working directory. An absolute path names a location on the
host running the command:

```yaml
source:
  path: /Applications/GarageBand.app
```

Host paths are machine-specific, so the content identity in the lockfile is what
travels. Symlinks behave as they do everywhere else: a link resolves to its target.
To snapshot selected files from a directory:

```yaml
source:
  resolver: local
  base: Assets
  include:
    - "**/*.ttf"
    - "**/*.otf"
```

`include` uses doublestar globs; each pattern must match at least one entry.
Overlapping patterns select each entry once. `base` defaults to the resource file's
directory and stays inside the project, because its contents are walked without
following symlinks. `file.path` is an exact path, not a glob. Native resolvers
reject fields belonging to another resolver.

Files and trees have content identities. Tree identity also includes permission
modes and supported symlinks. Packaging cannot silently discard required metadata;
see [filesystem limits](limitations.md#files-and-packages).

## Use a build output

```yaml
source:
  resource:
    apiVersion: stemma/v1alpha1
    kind: BuildMacPkg
    name: fonts
    output: installer
```

References include the kind and name, plus the named output. `apiVersion` defaults
to `stemma/v1alpha1`; supply it for external kinds. Required builds run before
their consumers even when only the consumer was selected on the command line.

## Understand the lock

| Command          | Input behaviour                                                                                   |
| ---------------- | ------------------------------------------------------------------------------------------------- |
| `update`         | Discover current inputs and atomically update their locks; does not publish                       |
| `prepare`        | Require matching locked declarations and local inputs; prepare artifacts                          |
| `plan` / `apply` | Require matching locked declarations and local inputs; fetch only the recorded remote observation |

A lock records the resolver and its version, the relevant declaration, a
resolver-owned observation and content identity. It is not merely a version
number or download URL. GitHub locks retain the selected release ID, asset ID,
release tag, download URL, filename and digest. Locked fetches replay that
concrete selection without evaluating the asset glob or looking up the latest
release again. Changing the pattern or release selector makes the declaration
stale and requires a lock update, even with cached bytes.

Homebrew, WinGet and GitHub assets with published hashes can update the lock
without downloading the file. The hash pins the required bytes; preparation
verifies them before use. A changed version or installer setting can therefore
appear in review even when the file itself is unchanged.

`update` also avoids downloads the cache or the lock can answer. A GitHub asset never
changes, so an asset the cache fetched before, or the one the lock records, is
not downloaded again. An HTTP URL can serve new bytes at any time: after
rediscovering a `match` link, `update` asks the server with the `ETag` and
`Last-Modified` the cache kept from its last download of that URL, and downloads
when the answer is new or the server sends no validators. The cache remembers
what it fetched whichever lock is checked out; the lock only decides whether
the result is a change. Runs that prepare fetch any content the cache no longer
holds.

Resources execute independently. Acquisition or preparation failures leave that
resource's complete reviewed lock entries unchanged; consumers of its outputs
are reported as blocked, with the resources that blocked them. Successful
resources can still update their entries. `update` can therefore write the
lockfile and exit nonzero after reporting every resource. A failed refresh stays
a failure even when the previous locked bytes remain cached.

A registry hash can be stale: resolution may succeed and preparation then fail.
Every consuming path verifies the required digest before using content. Offline
preparation requires cached bytes, not merely a resolved lock.

If a vendor replaces bytes at a stable URL, a cold locked run fails the content
check. It does not silently accept today's download. Run `update` and review the
change, or restore the locked bytes to the cache from a trusted copy.

Keep `stemma.lock.yaml` in Git. Destination descriptions, categories and other
metadata edits do not invalidate source acquisition or rebuild unchanged content.

## Cache and offline runs

```sh
stemma cache path
stemma prepare --offline
```

`--offline` requires verified cached network inputs and still checks local files.
It controls source and plugin acquisition; **it does not disable destination
network access for `plan` or `apply`**.

Repeated source lookups share cached responses and check with the publisher for
changes on later runs. A failed lookup remains a failure, even with cached data.

The disposable cache lives under `stemma` in the system's user cache directory
by default, outside the catalog. `stemma cache path` prints the effective location.
Set `STEMMA_CACHE_DIR` or `--cache-dir` when a runner needs another location, such
as a mounted cache volume. A repository-local cache is optional; ignore its path
in Git if you choose one.

`stemma cache prune` clears cached content after active runs finish. Locked remote
inputs can be fetched again if the publisher still serves the recorded bytes.
Prepared outputs depend only on input content, configuration and the stemma or
plugin build, so a new or recreated lock reuses them.

Destinations keep no local state either: each one identifies its publications
itself. See [publication identity and retention](publishing.md#identity-and-retention).
