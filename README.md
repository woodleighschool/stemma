# stemma 🌿

[![Release](https://img.shields.io/github/v/release/woodleighschool/stemma?display_name=tag&sort=semver)](https://github.com/woodleighschool/stemma/releases/latest)
[![CI](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/woodleighschool/stemma?logo=go)](https://github.com/woodleighschool/stemma/blob/main/go.mod)
[![License](https://img.shields.io/github/license/woodleighschool/stemma)](LICENSE)

A reproducible software artifact pipeline, run locally or in CI as one binary.

> **στέμμα** (_stémma_) — “wreath; lineage”

## 🚀 Usage

```sh
cp stemma.example.yaml stemma.yaml
mkdir -p software
cp software.example.yaml software/chrome.yaml
stemma validate
stemma prepare
stemma plan
stemma apply
```

Commit the Project, imported family documents and `stemma.lock.yaml`. `prepare`
records new inputs and local changes, then builds and inspects their content. It
reuses existing remote pins; `update` explicitly refreshes upstream discovery.
`plan` and `apply` require unchanged, reviewed locks. A cold cache
fetches the locked observation and verifies its bytes instead of rediscovering a
release. `--offline` requires cached network inputs and still checks local files.

Select a resource with `stemma prepare MacSoftware/chrome`; required build outputs
are prepared first. Use the full `apiVersion/Kind/name` when names are ambiguous.
`plan` reads destinations without mutating them. `prepare` stops before publication.
`--output json` reports resources, immutable artifacts and individual destinations.

Stages and diagnostics go to stderr. Terminals show the current stage and elapsed
time; redirected output and CI use ordinary log lines. `--no-progress` disables
animation. `--quiet` (`-q`) keeps warnings and errors, `--verbose` (`-v`) and
`--debug` (`-d`) enable debug diagnostics, and `--log-level` selects `debug`,
`info` (default), `warn` or `error`. These settings leave stdout reports intact.
Use `--log-format json` for structured stderr logs and `--output json` for the
final stdout report. Failed runs retain partial results and an `error`; absent
`lock_changed` means the lockfile comparison did not complete.

## 🌱 Documents

A root `Project` imports family YAML files. Each document has a literal
`apiVersion`, `kind`, `metadata.name` and `spec`; use `---` within a family file.
Asset paths are relative to the owning document. Components provide optional
map defaults through `extends`; lists and explicit nulls replace inherited values.

| Kind              | Authoring contract                                                                             |
| ----------------- | ---------------------------------------------------------------------------------------------- |
| `BuildMacPkg`     | Named inputs, payload layout, modes/ownership, package identity and endpoint installer scripts |
| `MacSoftware`     | One optional source, one application selection, Mac preparation and native destinations        |
| `WindowsSoftware` | One vendor installer or setup tree, optional accompanying files and native destinations        |

A Mac application needs one selection. Archive paths describe where to inspect;
`installed_path` describes its endpoint location. Bundle metadata, version and
supported icons derive from that selection. ZIP applications are wrapped as PKGs;
app DMGs retain the original image. `package_path` selects a nested vendor PKG
without reconstructing it. A source-free `MacSoftware` can publish Munki `nopkg`.

```yaml
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: editor
spec:
  source:
    url: https://code.visualstudio.com/sha/download?build=stable&os=darwin-arm64-dmg
    filename: VSCode.dmg
  application:
    path: Visual Studio Code.app
    installed_path: /Applications/Visual Studio Code.app
  destinations:
    munki:
      retention:
        keep: 1
      pkginfo:
        catalogs:
          - testing
```

Explicit construction stays separate from publication:

```yaml
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: fonts
spec:
  inputs:
    fonts:
      path: Assets/Fonts
  payload:
    /Library/Fonts:
      $input: fonts
      uid: 0
      gid: 0
      mode: "0755"
  package:
    identifier: edu.example.fonts
    version: "1.0"
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: fonts
spec:
  source:
    resource:
      kind: BuildMacPkg
      name: fonts
      output: installer
  destinations:
    munki:
      pkginfo:
        catalogs:
          - testing
```

Inputs accept `url`, `path`, or an explicit `resolver` with its configuration.
HTTP page matching and GitHub release discovery belong to their resolvers. Build
references identify a resource and named output; they are independent of native
installed-application dependencies. Files and trees are immutable. Tree identity
includes bytes, modes and confined symlinks; unsupported filesystem metadata fails
instead of being discarded. Payload ownership is explicitly authored and is never
implemented by changing ownership on the runner.

Windows setup content is part of `WindowsSoftware`, with no extra build document:

```yaml
source:
  url: https://update.code.visualstudio.com/latest/win32-x64/stable
  filename: VSCodeSetup.exe
content:
  setup_file: install.cmd
  files:
    install.cmd:
      path: windows/install.cmd
```

The Intune provider prepares the entire setup directory as `.intunewin`. A single
installer omits `content`. MSI descriptive fields, standard silent commands and
ProductCode/version detection derive from the selected MSI. Explicit native fields
win; EXE commands and detection are authored from vendor documentation. A
ProductCode rule only detects that ProductCode: use native file, registry or script
rules when detection must span major MSI upgrades. Intune script detection requires
exit zero, output on stdout and no stderr. File/registry version comparisons can
accept an already-newer installation.

The [generated schema](stemma.schema.json) describes the current interface.
`stemma schema --project --offline` incorporates installed plugin schemas and named
connections without loading destination credentials or downloading software.

## 📦 Publication

Connections live in the Project; each resource supplies native destination fields.
Explicit values override derived evidence. Omitted fields remain unmanaged, supported
nulls clear fields, and supplied lists replace their collections. Optional `$fact`
references select typed evidence such as `macos.application.app.version`; ordinary
application metadata and icon derivation require no individual fact references.

Intune supports native commands, context, requirements, detection and restart codes.
Dependencies and explicit supersedence refer to compatible Win32 app bindings on the
same connection. Releases update one bound app ID; supersedence connects separate
applications. Omitted relationship categories remain unmanaged; empty lists clear
them. No app retirement is inferred from supersedence.

Jamf uploads immutable packages and can maintain an exact patch-title association
and bound policy. Munki owns native pkginfo and installer objects. Destination
plugins define their own native modes: accepting PKG content does not imply accepting
`nopkg`, scripts, or another provider's deployment model.

`retention.keep` retains the current payload plus the N−1 most recently successfully
published distinct payloads, and anything still referenced. Protected older payloads
do not consume those slots. Metadata edits do not upload unchanged content or
advance publication order. Intune prunes inactive content versions; Jamf retires
owned obsolete associations before eligible packages; Munki retires version records
before unreferenced installers. Unknown ownership/order or incomplete reference
visibility blocks destructive cleanup.

The content cache is disposable. Keep `.stemma/state` durable, or set
`STEMMA_STATE_DIR`; losing bindings never authorizes adoption by name. Set
`STEMMA_CACHE_DIR` or `--cache-dir` to inspect or relocate the cache. Retained content
is neither a rollback promise nor an uninstall-file strategy.

## 🔌 Plugins

Plugins are trusted executables, selected by local `path` or registry `image`.
Declare each under Project `plugins` with `trusted: true`:

```yaml
plugins:
  catalog-tools:
    path: plugins/catalog-tools
    trusted: true
```

A path selects an executable file or a directory containing `plugin` (`plugin.exe`
on Windows). Set `entrypoint` to select another executable within a directory.
Relative paths resolve from the Project. Scripts use their executable shebang;
interpreters and other runtime dependencies must be installed on the runner.
`prepare` snapshots local files and records their content in the lockfile. Directory
snapshots include helper files. Local changes affect preparation identity, and
`plan`/`apply` reject changed code before executing it.

OCI images select a release tag or digest.
`stemma plugins install` records the index digest; `stemma plugins update` explicitly
changes pins. Only the current runner's bundle is fetched. Cold recovery uses the
locked digest. Registry authentication uses Docker/ORAS credentials; no container
runtime is required.

The [public SDK](plugin) defines the same registry and protocol used by built-ins:

- A resource operation registers its `apiVersion` and `kind`. `validate` separates
  named inputs, preparation configuration and native destinations; `run` receives
  leased locked inputs and produces named files or trees.
- A resolver registers a versioned observation contract. Discovery returns its
  observation; locked fetching must reproduce that content. Observations and
  declaration fingerprints exclude credentials.
- A destination advertises accepted content and owns native publication semantics,
  bindings and cleanup. It never needs a list of originating kind names.

`plugin.Stage(ctx, "Uploading installer")` reports activity and `plugin.Logger(ctx)`
provides a standard `slog.Logger`. Protocol version 3 streams bounded JSON log
records before the final response; `plugin.Serve` and `plugin.Run` handle framing
and log levels. Plugins reserve stdout for that protocol. Raw subprocess stderr
is discarded; log only deliberate diagnostics, never credentials or request bodies.

Artifacts carry optional typed facts and open, namespaced JSON evidence. The host
computes content identity, checks declared hashes and detects leased-input mutation.
Plugin bundle identity participates in preparation cache keys. Configuration and
metadata schemas are self-contained; external schema references cannot load code or
network resources. Credential fields use `writeOnly: true` so secret rotation does
not change connection identity.

Publish one tar.zst bundle per runner with `plugin` (`plugin.exe` on Windows) and
its resources at the archive root. Use OCI artifact type
`application/vnd.stemma.plugin.v1`, layer type
`application/vnd.stemma.plugin.bundle.v1.tar+zstd`, then combine platform manifests
in an OCI index. Workspaces are leases, not security sandboxes: trusted executables run with the caller's privileges.

## 🛠️ Runtime

Install the pinned development tools with `mise install`, then run `mise run deps`
and `mise run build`. The resulting CLI is a standalone binary. Target platform
and runner platform are separate; resource and provider descriptors declare concrete
runner requirements. Missing commands or unsupported runners fail before acquisition
or destination mutation, with setup instructions supplied by the operation.

| Operation                                           | Runner requirements and supported scope                                                                                     |
| --------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| HTTP/GitHub/local resolution, MSI inspection        | Go binary; no Windows runtime or installer execution                                                                        |
| Mac bundle/PKG inspection, HFS+/HFSX DMG extraction | Portable Go implementation; APFS and unsupported compression/layouts fail                                                   |
| `BuildMacPkg`, ZIP-app wrapping                     | Portable unsigned component PKGs; endpoint scripts are packaged, never executed; unsupported links/metadata fail            |
| Signature inspection                                | Portable supported PKG/Mach-O integrity and exact certificate pins; no Apple chain/revocation or native platform assessment |
| Automatic application icons                         | Embedded PNG and PNG-backed ICNS; extraction is optional                                                                    |
| `stemma icon` native rendering                      | macOS system image/Quick Look frameworks; PNG input remains portable                                                        |
| Intune Win32 preparation                            | Existing portable Go wrapper, bounded to 2 GiB; no .NET requirement                                                         |
| Microsoft's comparison tool                         | Windows and .NET Framework 4.7.2, as documented by Microsoft                                                                |

The portable wrapper has independent format verification. Packaging or API acceptance
does not prove endpoint installation. Microsoft's tool is not invoked implicitly;
its runtime is needed when using it as an external compatibility check.

## 🧑‍💻 Development

```sh
mise run generate
mise run test
mise run lint
mise run build
```

`mise run generate-graph` regenerates the scoped, pinned Intune clients.

## 📄 License

[Apache-2.0](LICENSE), with MIT-licensed portions identified in their source headers.

## 🙏 Credits

- [WrapTune-MacOS](https://github.com/thefinder808/WrapTune-MacOS) — Windows packaging and verification reference
- [Fleet](https://github.com/fleetdm/fleet) — BOM and XAR package writers
- [mholt/archives](https://github.com/mholt/archives) — archive handling
