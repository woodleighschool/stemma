---
slug: /
---

# Stemma

Stemma is a cross-platform software artifact pipeline. It downloads software,
prepares installers and publishes them to software distribution systems.

A **catalog** contains your software definitions, assets and destination settings.
Stemma reads that catalog; it does not supply a collection of applications itself.
[Our catalog](https://github.com/woodleighschool/stemma-catalog) is one example of
how to organise those files.

## Start here

- [Getting started](getting-started.md): prepare an application and see what would be published.
- [Mac software](mac-software.md): vendor PKGs, applications in ZIPs and DMGs, and script-only items.
- [Windows software](windows-software.md): MSI and EXE installers, accompanying files and Intune detection.
- [Building a Mac package](building-packages.md): fonts, branding and other custom payloads.
- [Using plugins](plugins.md) and [writing your own](writing-plugins.md): local executables, resolvers, resource kinds and destinations.

## How a run works

1. The catalog declares an input and the installation settings for each destination.
2. Stemma resolves the input and records its identity in `stemma.lock.yaml`.
3. It inspects or prepares an immutable artifact: a file or a directory tree.
4. Each destination uses that artifact and its metadata to plan or apply native changes.

Mac and Windows software share input resolution, locks, caching and publication
state. Their installation settings stay platform-specific. For example, an MSI
ProductCode and a Mac bundle identifier describe different things.

Stemma is under active development. Read the [current limitations](limitations.md)
before relying on a format or deployment mode. It is a command you invoke; it
runs no service and does not continuously reconcile devices. A scheduler can
invoke [`stemma reconcile`](reconcile.md) to keep a catalog repository published
and propose updates as pull requests.

## Coming from AutoPkg

The [AutoPkg wiki](https://github.com/autopkg/autopkg/wiki) remains the reference for
AutoPkg. These are useful starting points when writing a Stemma catalog:

| In AutoPkg                            | In Stemma                                                                   |
| ------------------------------------- | --------------------------------------------------------------------------- |
| Recipe repository                     | Catalog: a Project plus imported YAML documents and assets                  |
| Download recipe and version discovery | A source resolver and its reviewed lock entry                               |
| Application inspection processors     | One application selection with derived metadata; icons are committed assets |
| Package construction recipe           | `BuildMacPkg`, when you need to construct a payload                         |
| Munki import recipe                   | Native destination settings on `MacSoftware`                                |
| Shared recipe inputs and overrides    | Optional components and explicit values                                     |
| Custom processors                     | Trusted plugins registering operations or resource kinds                    |

An AutoPkg processor chain need not become a chain of Stemma documents. A vendor
application normally needs one software document. Custom package construction is
separate because the resulting package can be consumed by more than one publisher.
