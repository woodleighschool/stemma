# stemma 🌿

[![Release](https://img.shields.io/github/v/release/woodleighschool/stemma?display_name=tag&sort=semver)](https://github.com/woodleighschool/stemma/releases/latest)
[![CI](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/woodleighschool/stemma?logo=go)](https://github.com/woodleighschool/stemma/blob/main/go.mod)
[![License](https://img.shields.io/github/license/woodleighschool/stemma)](LICENSE)

A reproducible software artifact pipeline, run locally or in CI as one binary.

> **στέμμα** (_stémma_) — “wreath; lineage”

## 🌱 What's inside

- Locked HTTP, GitHub and local inputs, organised by software family
- Shared preparation, portable PKG and Intune Windows packaging
- Native Intune, Jamf and Munki destinations
- Additional destinations through plugins

## 🚀 Usage

```sh
cp stemma.example.yaml stemma.yaml
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

`prepare` stops before publication; `inspect` reads artifact facts.
Use `package --help` for standalone packaging and `completion` for shell setup.
`stemma icon App.app --out icons/App.png` retains a macOS-rendered PNG;
`--refresh` explicitly replaces it.

## ⚙️ Configuration

The [generated schema](stemma.schema.json) provides editor validation and hover
descriptions.

Keep shared settings in the root and import `software/**/stemma.yaml` for
software families and their assets. Components apply defaults; named artifacts
share builds across destinations. Included local file changes invalidate the lock.
Package timestamps use the source's recorded lock time.

Native metadata retains field ownership: omission preserves existing values,
`null` clears supported fields, and lists replace whole collections.
Omitted fields inside objects remain unmanaged.

Declare trusted plugin binaries, then run `stemma plugins install` or
`stemma plugins update`. Plugins handle `plan` and `apply`.
Persist `.stemma/state` separately from the disposable cache, or set
`STEMMA_STATE_DIR`. Credentials are referenced by environment-variable name.

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
