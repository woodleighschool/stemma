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
mise run vulncheck
```

`mise run generate` refreshes `stemma.schema.json` from the Go contracts.
`mise run generate-graph` regenerates the scoped Microsoft Graph clients. Keep
generated outputs with changes to their source contracts.

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
