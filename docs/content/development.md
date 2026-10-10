# Development

Mise owns the tool versions and commands:

```sh
mise install
mise run deps
mise run build
```

The build writes `stemma` in the repository root. Use `mise tasks` for the available
tasks. The [repository guidance](https://github.com/woodleighschool/stemma/blob/main/AGENTS.md)
describes code and contribution conventions.

## Checks and generated files

```sh
mise run format
mise run lint
mise run test
mise run build
mise run generate
mise run vulncheck
```

`mise run generate` writes the default editor schema to
`docs/static/stemma.schema.json` from the registered built-in operations. Commit it
with contract changes; the docs build publishes it as `/stemma/stemma.schema.json`
on GitHub Pages. `stemma schema --output-file stemma.schema.json` generates a
catalog-specific schema from the same contracts and the locally loaded plugins.
`mise run generate-graph` regenerates the scoped Microsoft Graph clients. Keep
generated outputs with changes to their source contracts.

## Benchmarks

```sh
mise run bench
mise run bench-codspeed
```

Both commands run benchmarks without correctness tests, one package at a time,
with four Go scheduler threads. `bench` reports native Go timings and allocations;
`bench-codspeed` replaces the CodSpeed walltime data in `tmp/codspeed` without uploading.
Fixture generation and output cleanup are outside the timed operations.

The suite covers canonical tree acquisition, cache streams, physical and TAR-backed
signature verification, PKG creation/inspection/extraction, DMG creation/opening/reading,
and application update/cold/warm preparation. Packaging uses generated bundles with
256 resources and a 17 MiB executable crossing a PBZX block boundary. An additional
XZ build mixes zero-filled and varied deterministic data to exercise compression
beyond repeated blocks. Signature verification uses the existing signed fixtures.
No installed applications or external fixture downloads are required.

`STEMMA_BENCH_APP=/Applications/Example.app` selects an installed application for
acquisition, verification and preparation measurements. Keep those local results
separate from CI's fixed workload. Cold preparation means an empty Stemma cache;
warm preparation still reads the complete source to enforce its content lock.
Neither benchmark flushes the operating-system cache.

The Benchmarks workflow reports PR and main-branch measurements through the
[CodSpeed GitHub integration](https://codspeed.io/docs/integrations/providers/github).
PR comparisons use the main-branch baseline.
Renovate updates the action pin and the Go integration's Mise pin;
CI reads the latter from the local benchmark task.
CI measures walltime on CodSpeed ARM64 Macro Runners, including I/O. Compare runs
from the same runner type. CodSpeed does not report Go allocations; use `bench`
for those. Linux measurements do not reproduce macOS endpoint-protection overhead.
Regressions are visible only in the operations and workloads exercised by the suite.

## Work on the docs

```sh
mise run docs-serve
mise run docs
```

The preview prints its local URL. The build writes `docs/build/` and checks
formatting, TypeScript, Markdown and internal links. Pages live in `docs/content/`;
navigation and theme settings live beside them in `docs/`.

The site uses Docusaurus with pnpm, local search and the same theme as Woodstar's
docs. Dependencies are pinned in `docs/pnpm-lock.yaml`. Run `mise run //docs:deps`
to install them, or `mise run //docs:format` to format the site sources.

Pull requests build the site; main-branch docs changes publish through GitHub Pages.
