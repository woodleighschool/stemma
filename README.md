# stemma 🌿

[![Release](https://img.shields.io/github/v/release/woodleighschool/stemma?display_name=tag&sort=semver)](https://github.com/woodleighschool/stemma/releases/latest)
[![CI](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/woodleighschool/stemma/actions/workflows/ci.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/woodleighschool/stemma?logo=go)](go.mod)
[![License](https://img.shields.io/github/license/woodleighschool/stemma)](LICENSE)

Stemma is a cross-platform software artifact pipeline for preparing and publishing software.

Build macOS packages from Windows, create Intune `intunewin` files from macOS or Linux, and publish software to Munki, Intune or Jamf. Catalogs define the software Stemma manages, while plugins can extend the pipeline with additional sources, processing and destinations.

> [!WARNING]
> This project may be unstable or have bugs, use with caution.
> Also expect breaking changes between releases for now.

## 🌱 What's inside

- Build macOS packages and Intune `intunewin` files on macOS, Linux or Windows
- Download software from upstream sources and lock resolved inputs
- Extract application metadata, versions and icons
- Prepare artifacts for different deployment platforms
- Publish to Munki, Intune and Jamf
- Preview destination changes before applying them
- Reconcile a catalog on a schedule, publishing reviewed changes and proposing upstream updates as pull requests
- Extend sources, processing and destinations through plugins

## 🚀 Usage

Start with [stemma-catalog](https://github.com/woodleighschool/stemma-catalog).
Follow its setup instructions, then run from the catalog:

```bash
stemma prepare
stemma plan
```

`prepare` downloads and prepares the software; `plan` shows what would be published.
Use `stemma apply` to publish after reviewing the result.

## 📖 Documentation

- [Getting started](docs/content/getting-started.md)
- [Starting a catalog](docs/content/catalogs.md)
- [Mac software](docs/content/mac-software.md) and [Windows software](docs/content/windows-software.md)
- [Creating plugins](docs/content/writing-plugins.md)

## 🧑‍💻 Development

```bash
mise install
mise run deps
mise run build
```

See [development](docs/content/development.md) for checks, generation and the docs preview.

## 📄 License

Licensed under the [Apache License 2.0](LICENSE).

## 🙏 Credits

- **[WrapTune-MacOS](https://github.com/thefinder808/WrapTune-MacOS)** - Windows packaging and verification reference
- **[Fleet](https://github.com/fleetdm/fleet)** - XAR package writer
- **[mholt/archives](https://github.com/mholt/archives)** - archive handling
- **[AutoPkg](https://github.com/autopkg/autopkg)** - inspiration for software packaging workflows
