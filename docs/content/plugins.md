# Using plugins

Plugins are executables that add resource kinds, input resolvers or destinations.
They use the same artifact and operation contracts as built-ins. A catalog can
carry its own plugin files or select a published OCI bundle.

Our catalog's [plugin setup](https://github.com/woodleighschool/stemma-catalog/blob/main/stemma.yaml)
shows configured bundles. The [downloads plugin](https://github.com/woodleighschool/stemma-catalog/tree/main/plugins/downloads)
includes vendor resolvers and instructions for both local builds and published
bundles. To add your own operation, see [creating plugins](writing-plugins.md).

## Load a local plugin

Add it to the Project:

```yaml
spec:
  plugins:
    catalog-tools:
      path: plugins/catalog-tools
```

`path` selects an executable or a directory containing `plugin` (`plugin.exe` on
Windows). A directory can include helper files. Set `entrypoint` to a relative
path within that directory if its executable has another name.

Relative paths resolve from the Project. Absolute paths can select code outside
the catalog. Stemma does not build a plugin from a source checkout: build its
executable first, then point `path` at the executable or bundle directory.

Executable scripts can implement the protocol too. On Unix they need executable
permissions and a working shebang. Interpreters and helper tools must be available
on the runner.

Run `stemma plugins update` to snapshot the selected files and lock their
content. Changing the executable or bundled helpers changes plugin identity: the
plugin does not load, and commands that use it fail, until `stemma plugins update`
or `stemma update` locks it again. Local plugins need no registry or container
runtime.

## Load an OCI bundle

```yaml
spec:
  plugins:
    catalog-tools:
      image: ghcr.io/example/catalog-tools:1.0.0@sha256:0f5c…
```

These are executable bundles, not arbitrary container images, and need no
container runtime. Stemma fetches the platform index the image names and the
current runner's bundle from it.

An image that names its digest selects its code by itself and needs no lock
entry. A tag without a digest is locked to the index it names when
`stemma plugins update` or `stemma update` resolves it. Other commands keep using
the locked index after the tag moves; `stemma plugins update` resolves the tag
again. Stemma never moves a declaration to another release.

Private registries use Docker/ORAS registry credentials. `stemma plugins publish`
creates these indexes; see [creating plugins](writing-plugins.md#distribute-a-bundle).

## Inspect plugins

```sh
stemma plugins list
stemma plugins inspect
```

`plugins list` reads declarations and recorded lock entries without executing
plugins. It identifies unlocked or changed declarations; a recorded pin does not
verify that local files still match.

`plugins inspect` loads the pinned code and describes it: the code it runs, its version and VCS revision, the runner platforms
an image has bundles for, and the resolvers, resource kinds and destinations it
offers. A plugin that does not load, or offers operations this Stemma cannot use,
is reported with the reason, and the command exits non-zero. `--json` prints the
complete report.

```text
catalog-tools
  Image:           ghcr.io/example/catalog-tools:1.0.0@sha256:0f5c2e9b7a41
  Version:         1.0.0 (58d5d19c08d2)
  Platforms:       darwin/arm64, linux/amd64, windows/amd64
  Resolvers:       vendor-feed
  Resource kinds:  example.org/v1/VendorPackage
```

`stemma schema --output-file -` includes the schemas of every operation.

## Updates

A declaration that names its digest suits automated updates: the tag and digest
move together in `stemma.yaml` and the lockfile does not change. Renovate's docker
datasource can do this with a regex manager:

```json5
{
  customManagers: [
    {
      customType: "regex",
      managerFilePatterns: ["/(^|/)stemma\\.yaml$/"],
      matchStrings: [
        "image:\\s*[\"']?(?<depName>[^\\s\"'@]+):(?<currentValue>[^\\s\"'@:/]+)@(?<currentDigest>sha256:[a-f0-9]{64})",
      ],
      datasourceTemplate: "docker",
    },
  ],
}
```

`stemma validate` then loads the new plugin and fails if the catalog uses an
operation it cannot provide. Without automation, declare a tag without a digest and run
`stemma plugins update` whenever you want the index it names now.

## Compatibility

Each plugin names the interface version of every operation kind it implements:
resolvers, resource kinds and destinations. Stemma uses a kind's operations only
while the plugin's version matches its own, so a plugin built for another Stemma
keeps the operations whose versions still match. The others fail only the
commands that use them, naming both versions. A plugin that does not load fails
the same way. `stemma schema` describes every operation, so it fails until all of
them are usable; the `describe` tool of [`stemma mcp`](commands.md#agents) lists
the unusable ones beside the others.

## Configure an operation

The plugin's descriptor supplies operation names, resource kinds, schemas and
runner requirements. Installing a plugin does not automatically configure a
destination. Add a named connection using the advertised operation, then put its
native metadata in the software document.

`stemma schema --offline --output-file stemma.schema.json` includes their schemas
for your editor, and the `describe` tool of `stemma mcp` outlines them for agents.

Declaring a plugin selects executable code that runs with the caller’s privileges,
including when loading contracts for validation. Workspaces isolate working files
but are not a security sandbox. Review plugin declarations and lock changes as
executable dependencies. See [creating plugins](writing-plugins.md) for the SDK and wire
contract.
