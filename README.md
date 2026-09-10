# stemma 🌿

[![Release](https://img.shields.io/github/v/release/woodleighschool/stemma?display_name=tag&sort=semver)](https://github.com/woodleighschool/stemma/releases/latest)
[![CI](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/woodleighschool/stemma?logo=go)](https://github.com/woodleighschool/stemma/blob/main/go.mod)
[![License](https://img.shields.io/github/license/woodleighschool/stemma)](LICENSE)

A reproducible software artifact pipeline, run locally or in CI as one binary.

> **στέμμα** (_stémma_) — “wreath; lineage”

## 🌱 What's inside

- Locked HTTP, GitHub and local inputs, organised by software family
- Preserved application, package and MSI facts with explicit native composition
- Shared preparation, named operation steps, portable PKG and Intune Windows packaging
- Native Intune, Jamf and Munki destinations
- Trusted executable plugins providing inspection, preparation or reconciliation operations

## 🚀 Usage

```sh
cp stemma.example.yaml stemma.yaml
mkdir -p software
cp software.example.yaml software/chrome.yaml
stemma validate
stemma operations
stemma update
stemma plan
stemma apply
```

Commit `stemma.yaml`, software files and `stemma.lock.yaml`.

| Option                 | Purpose                                       |
| ---------------------- | --------------------------------------------- |
| `--frozen-lockfile`    | Require a matching lockfile; default in CI    |
| `--no-frozen-lockfile` | Permit missing or changed inputs              |
| `--refresh`            | Refresh sources during a run                  |
| `--no-lockfile`        | Ignore the source lockfile without writing it |
| `--offline`            | Require cached source inputs                  |
| `--output json`        | Machine-readable reports                      |

`validate` checks configuration and operation contracts before software acquisition;
content-dependent requirements are checked after inspection. `operations` prints
the built-in and trusted plugin catalog. Both accept `--offline` for cached plugin
binaries. `prepare` stops before publication; `inspect` reads artifact facts.
Use `package --help` for standalone packaging and `completion` for shell setup.
`stemma icon App.app --out icons/App.png` retains a macOS-rendered PNG;
`--refresh` explicitly replaces it.

## ⚙️ Configuration

The [generated schema](stemma.schema.json) provides editor validation and hover
descriptions. `stemma schema --project --offline` includes the configured plugins'
schemas from verified cached bundles. Save its output for the editor; it needs no
destination credentials or software downloads.

Author one `apiVersion: stemma/v1alpha1`, `kind: Software` document per managed item.
A family file can contain several platform documents separated by `---`.
`metadata.name` is its stable identity and `spec` owns acquisition and native delivery.
The root `kind: Project` uses `metadata.name` for project identity and keeps imports,
connections, plugins and component defaults under `spec`. Import paths such as
`software/**/*.yaml` select family document streams; paths to assets are relative to
the owning document. Components apply defaults. Included local file changes
invalidate the lock; destination metadata edits preserve acquisition and preparation
results. Package timestamps use the source's recorded lock time. Omit `source`
when the managed item needs no acquisition; it then has no source lock entry or
implicit `source` and `prepared` outputs.

Sources use built-in HTTP, GitHub, file or local providers. An HTTP `match`
expression selects one distinct complete artifact URL from a download page;
the lock pins that URL and its bytes. Frozen recovery uses the locked URL without
resolving the page again. Named steps compose
registered operations; each step can consume `source`, `prepared`, `artifacts/name`
or an earlier `step/output`. The `artifacts` shorthand declares shared PKG builds;
steps expose the operation's named outputs directly:

```yaml
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: branding
spec:
  source:
    type: local
    include:
      - Payload/**
      - Scripts/postinstall
  steps:
    - name: package
      operation: pkg
      inputs:
        input: prepared
      config:
        identifier: org.example.branding
        version: "1.0"
        payload: Payload
        scripts:
          postinstall: Scripts/postinstall
  destinations:
    munki:
      installer: package/artifact
      retention:
        keep: 1
      pkginfo:
        catalogs:
          - testing
```

Named subjects select observed evidence without changing the delivered artifact.
Selectors accept an exact artifact `path`, observed `installed_path`, `bundle_id`,
or a combination; they must match exactly one subject. Providers derive native
metadata from that evidence, with explicit native fields taking precedence:

```yaml
subjects:
  app:
    kind: app
    path: Example.app
destinations:
  munki:
    installer: source
    derive:
      app:
        subject: app
        installed_path: /Applications/Example.app
    pkginfo:
      name: Example
      catalogs:
        - testing
```

The Munki provider owns `pkginfo` and `derive`. Derivation builds
complete application detection entries, including both observed version strings.
`version_key: CFBundleVersion` chooses the build for the native version comparison.
Local Munki catalogs use `pkginfo.catalogs`. Copying one app from a DMG defaults to `/Applications`; package
payload detection uses the observed installation path or an authored override.

Typed `$fact` references select individual native values, for example
`version: {$fact: app.app.build}` inside `pkginfo`. Objects merge recursively;
provided lists replace their collections. Explicit `false`, `[]` and supported
`null` retain their meaning. Omitted fields outside derivation remain unmanaged.
`unmanaged` lists native field paths to exclude from derivation, such as
`pkginfo.minimum_os_version`. Removing `derive` relinquishes its ownership.
Disappearing optional facts clear prior generated Munki fields; Intune requires an
explicit replacement or `unmanaged` when the native clear is not established.

A source-free Munki item directly supplies `pkginfo` with
`installer_type: nopkg`, `name`, `version` and native scripts. Scripts are metadata
for endpoints and never execute on the runner. `munki.pkginfo` remains an optional
JSON rendering operation for explicit artifact composition; ordinary publication
needs no renderer step.

Intune accepts native Graph fields with `type: win32`, `pkg` or `dmg` as subtype
shorthands. `derive.msi` names the subject supplying observed descriptive and MSI identity
fields from a named subject. Windows commands, requirements, detection, return
codes and assignments remain authored. The Win32 provider packages raw MSI/EXE
inputs into `.intunewin` internally. Relationship declarations use Software names:

```yaml
dependencies:
  - software: vc-runtime
    auto_install: true
supersedes:
  - software: legacy-client
    uninstall_previous: true
```

Relationships resolve durable app bindings on the same named connection. Selected
items run in reference order. A missing binding fails; omitted relationship
categories remain unmanaged and an empty list clears that category. Ordinary
releases update the existing app ID. Supersedence connects explicitly separate
app objects and does not automatically retire the old app.

Jamf publishes immutable package IDs. Its optional `patch` associates a package
with one exact title version and maintains a bound native policy:

```yaml
patch:
  title_configuration_id: "42"
  version:
    $fact: app.app.version
  policy:
    name: Chrome updates
    enabled: false
    scope:
      all_computers: false
      computers: []
      computer_groups: []
```

`retention.keep` retains the current payload and the N−1 most recently
successfully published distinct payloads, plus anything still referenced. Protected
older payloads do not consume those slots. Providers record success after content
and intended reference updates complete; metadata edits do not advance publication
order. Intune prunes inactive content versions, Jamf retires owned obsolete title
associations before deleting eligible packages, and Munki retires version
records before unreferenced installer content. Unknown ownership/order and
incomplete reference visibility block destructive cleanup. Keep durable bindings
independently of cache; retained content is neither deployment rollback nor an
uninstall-file strategy.

## 🔌 Operations

Plugins are trusted executable providers, separate from source acquisition and
destination connections. Each OCI release supplies bundles for its supported runners. Configure plugins and
connections under the Project document’s `spec`:

```yaml
plugins:
  inventory:
    trusted: true
    image: ghcr.io/example/inventory:v1.0.0
destinations:
  inventory:
    operation: inventory.reconcile
    config:
      token: ${INVENTORY_TOKEN}
```

Run `stemma plugins install` to pin the release index in `stemma.lock.yaml`.
Only the current runner's bundle is fetched. Tags move only on explicit
`stemma plugins update`; normal runs and cold cache recovery use the locked digest.
Registry authentication uses the standard Docker/ORAS credential store and helpers.
No container runtime is required.
The [Go SDK](plugin) uses protocol v2: one executable advertises multiple named
operations with input/output JSON Schemas, runner requirements, methods and side
effects. Providers with constrained configuration declare `config_schema` and `metadata_schema`
so authored fields can be checked before runtime artifacts exist. Built-in and
external names share one registry; collisions fail. Schemas are self-contained
and cannot load network or filesystem references.
Providers can declare `requires_inspection` to receive observed artifact facts;
Stemma does not interpret their metadata to decide what evidence they need.

Preparation steps support `validate` and `run` with `none` or `workspace` side
effects. Reconciliation operations support `validate`, `plan` and `apply`.
Plugins execute with the caller's privileges; workspaces are leases, not sandboxes.
Failed reconciliation responses can retain bindings for completed remote work.
Persist `.stemma/state` separately from the disposable cache, or set `STEMMA_STATE_DIR`.
Supply credentials as literal configuration values using `${VAR}` expansion.
Mark credential fields `writeOnly: true` in the connection schema so rotating them
preserves the destination's durable bindings.

### Publishing a plugin

Build a standalone executable for each supported OS/architecture pair. Package
it as `plugin` (`plugin.exe` on Windows) at the root of one tar.zst archive,
alongside any resources and licences. Resolve resources relative to the executable.
Normalize archive ordering, ownership, modes and timestamps for reproducible bundles.

Publish each bundle with ORAS, then assemble one OCI platform index:

```sh
oras push ghcr.io/example/inventory:v1.0.0-linux-amd64 \
  --artifact-platform linux/amd64 \
  --artifact-type application/vnd.stemma.plugin.v1 \
  --annotation org.opencontainers.image.created=1970-01-01T00:00:00Z \
  plugin.tar.zst:application/vnd.stemma.plugin.bundle.v1.tar+zstd
oras manifest index create ghcr.io/example/inventory:v1.0.0 v1.0.0-linux-amd64
```

Add platform tags to the index command for Darwin, Linux or Windows on amd64 or
arm64. Each manifest contains exactly one bundle. The index selects the runner;
the executable's `describe` response owns operation contracts. The selected
manifest digest identifies the implementation, so resource changes invalidate
cached operation output. External providers own their native schema and rendering;
they receive the same facts, subject selectors and durable bindings through the
public protocol.

## 🔎 Artifact support

Inspection is static and never runs payloads or installer hooks. Facts preserve
container relationships, observed paths and each subject's original versions.

| Artifact           | Supported behaviour                                                                        |
| ------------------ | ------------------------------------------------------------------------------------------ |
| Application bundle | Bundle identifier, short/build versions, executable and minimum OS                         |
| PKG                | Component receipts and payload application facts with containment and installation paths   |
| MSI                | Database identities, product version and native properties                                 |
| EXE or other file  | Exact content identity; installed application behaviour requires authored native rules     |
| DMG                | Portable HFS+/HFSX inspection and selection of an app or flat PKG; original image retained |
| `.intunewin`       | Transport envelope inspection and packaging, separate from installed software facts        |

Portable PKG creation retains payloads and scripts without executing them.
ZIP applications can become PKGs with `pkg` and an explicit installation path.
App DMGs use the original `source` with native `copy_from_dmg`, `items_to_copy`
and installed application detection. A selected inner PKG uses `prepared` and
retains the vendor's installer bytes and receipts. Disk images are never mounted;
APFS, unsupported compression and unrepresentable payload metadata are rejected.
Required unsupported layouts or verification capabilities fail before publication.
`verification.subject` selects `source`, `payload`, `prepared`, `artifacts/name`
or `step/output`; evidence remains attached to that artifact. This allows an
installer to be verified independently of a rendered metadata document.
Application signature checks authenticate each architecture's primary SHA-256
CodeDirectory using detached CMS. Alternate CodeDirectories and nested resource
sealing remain unsupported. Certificate pins authenticate the exact signer;
Apple chain trust, revocation and native platform assessment are separate checks.

String values under `spec` support whole-value environment placeholders, for example
`token: ${GITHUB_TOKEN}` or `client_secret: ${JAMF_CLIENT_SECRET}`. Export the
variables before running commands. Unset variables fail configuration loading;
values remain strings, including empty strings. Resource headers (`apiVersion`,
`kind` and `metadata.name`) are literal and never depend on the runner environment.
Embedded expressions remain literal, including shell expressions inside scripts.
Mapping keys remain literal. Source tokens and
native destination authentication secrets do not affect lock or binding identity.

## 🧑‍💻 Development

```sh
mise install
mise run build
mise run generate
mise run test
mise run lint
```

`mise run generate-graph` regenerates the pinned Kiota clients for Intune. We scope them to
avoid the full Microsoft Graph SDKs’ high memory use during cold builds.

## 📄 License

[Apache-2.0](LICENSE), with MIT-licensed portions identified in their source headers.

## 🙏 Credits

- [WrapTune-MacOS](https://github.com/thefinder808/WrapTune-MacOS) — Windows packaging and verification reference
- [Fleet](https://github.com/fleetdm/fleet) — BOM and XAR package writers
- [mholt/archives](https://github.com/mholt/archives) — archive handling
