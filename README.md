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
descriptions.

Author one `apiVersion: stemma/v1alpha1`, `kind: Software` document per managed item.
`metadata.name` is its stable identity and `spec` owns acquisition and native delivery.
The root `kind: Project` uses `metadata.name` for project identity and keeps imports,
connections, plugins and component defaults under `spec`. Import paths such as
`software/**/*.yaml` select individual documents; paths to assets are relative to
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
      artifact: package/artifact
      catalogs:
        - testing
```

The following snippets belong under a Software document’s `spec`.
Named subjects select observed evidence without changing the delivery artifact.
Typed `$fact` references resolve native values, preserving their types and failing
on missing or ambiguous subjects. For example, a Software document can choose an application's
build for Munki while retaining its short version and package receipt version:

```yaml
subjects:
  main:
    kind: app
    bundle_id: org.example.app
destinations:
  munki:
    version:
      "$fact": main.app.build
```

Native defaults derive from observed facts, then explicitly supplied metadata takes
precedence. Objects merge recursively; lists replace collections, including generated
detection lists. Explicit `false`, `[]` and supported `null` retain their meaning.
Fields outside derived or explicit ownership remain unmanaged.

`munki.pkginfo` renders native metadata as a JSON artifact. A local repository or
external transport can consume that document alongside the original installer:

```yaml
steps:
  - name: contents
    operation: inspect
    inputs:
      input: prepared
  - name: metadata
    operation: munki.pkginfo
    inputs:
      input: contents/artifact
    config:
      name: Example
      description: Our managed application
destinations:
  munki:
    artifact: metadata/artifact
    inputs:
      installer: contents/artifact
    catalogs:
      - testing
```

The renderer derives receipt and version defaults from the installer. Its `config`
accepts native fields and typed fact references. Destination `inputs` names extra
artifacts; neither these references nor `artifact` become native metadata.
The local Munki operation also accepts an installer directly.

For a [script-only Munki item](https://github.com/munki/munki/wiki/nopkgs), omit
the source and renderer inputs, and supply `installer_type: nopkg`, `name`,
`version` and the native scripts in the renderer's `config`. Destinations consume
the resulting JSON artifact without an `inputs.installer`. No installer is built
or uploaded; scripts remain desired metadata and are never executed by Stemma.

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
effects. Providers with constrained configuration also declare a `config_schema`
so authored fields can be checked before runtime artifacts exist. Built-in and
external names share one registry; collisions fail. Schemas are self-contained
and cannot load network or filesystem references.

Preparation steps support `validate` and `run` with `none` or `workspace` side
effects. Reconciliation operations support `validate`, `plan` and `apply`.
Plugins execute with the caller's privileges; workspaces are leases, not sandboxes.
Failed reconciliation responses can retain bindings for completed remote work.
Persist `.stemma/state` separately from the disposable cache, or set `STEMMA_STATE_DIR`.
Supply credentials as literal configuration values using `${VAR}` expansion.

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
cached operation output. [Woodstar](https://github.com/woodleighschool/woodstar/tree/main/stemma)
uses GoReleaser archives and its ORAS publisher for this contract.

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
