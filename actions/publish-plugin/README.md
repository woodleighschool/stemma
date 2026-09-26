# publish-plugin

Publishes the plugin bundles from a GoReleaser release as a Stemma OCI plugin with
`stemma plugins publish`.

## Usage

Log in to the registry, run GoReleaser, then publish:

```yaml
jobs:
  release:
    name: Release
    runs-on: ubuntu-26.04
    permissions:
      contents: write
      packages: write
    steps:
      - name: Checkout
        uses: actions/checkout@<sha> # v7.0.1
        with:
          fetch-depth: 0
          persist-credentials: false

      - name: Log in to GHCR
        uses: docker/login-action@<sha> # v4.6.0
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@<sha> # v7.2.3
        with:
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}

      - name: Publish plugin
        uses: woodleighschool/stemma/actions/publish-plugin@<sha> # 0.3.0
        with:
          image: ghcr.io/example/catalog-tools:${{ github.ref_name }}
```

Reference the action by the commit SHA of a Stemma release. A branch or tag reference trips
zizmor's `unpinned-uses` audit in the calling repository.

## Inputs

| Input            | Default            | Description                                          |
| ---------------- | ------------------ | ---------------------------------------------------- |
| `image`          |                    | Plugin image reference with the release tag          |
| `dist`           | `dist`             | GoReleaser dist directory, relative to the workspace |
| `stemma-version` | The pinned release | Stemma release that publishes the plugin             |

## Behaviour

- GoReleaser's `artifacts.json` decides the platforms: every tar.zst archive it built becomes one
  platform of the index. Set `dist` to `<workdir>/dist` when GoReleaser runs in a subdirectory.
- Stemma checks every bundle before pushing and tags the index last. Rerunning a release with the
  same bundles uploads nothing new and keeps the digest.
- Registry credentials come from the Docker credential store, so the action works with any
  registry a login step has signed in to.
- The index carries `org.opencontainers.image.source` for the calling repository, which links a
  GHCR package to it.
- Stemma comes from its GitHub release through Mise's `github:` backend, so the job needs no Go
  toolchain for this step. The default version is the Stemma release the pinned commit belongs
  to, and moving the pin moves Stemma with it.
