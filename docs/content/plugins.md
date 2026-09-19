# Using plugins

Plugins are executables that add resource kinds, input resolvers or destinations.
They use the same artifact and operation contracts as built-ins. A catalog can
carry its own plugin files or select a published OCI bundle.

## Load a local plugin

Add it to the Project:

```yaml
spec:
  plugins:
    catalog-tools:
      path: plugins/catalog-tools
      trusted: true
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

Run `stemma prepare` to snapshot the selected files and record their content in
the lockfile. Changing the executable or bundled helpers changes plugin identity.
Frozen `plan` and `apply` runs reject changed code. Create or refresh local plugin
pins with `prepare` before using `--offline`. Local plugins need no registry or
container runtime.

## Load an OCI bundle

```yaml
spec:
  plugins:
    catalog-tools:
      image: ghcr.io/example/catalog-tools:1.0.0
      trusted: true
```

Replace the example with an actual Stemma plugin reference containing a tag or
digest. These are executable bundles, not arbitrary container images. A container
runtime is not required.

```sh
stemma plugins install
stemma plugins list
stemma operations
```

Installation pins the OCI index digest and downloads the current runner's bundle.
Use `stemma plugins update` to move existing pins. A cold cache uses the locked
digest. Private registries use Docker/ORAS registry credentials.

## Configure an operation

The plugin's descriptor supplies operation names, resource kinds, schemas and
runner requirements. Installing a plugin does not automatically configure a
destination. Add a named connection using the advertised operation, then put its
native metadata in the software document.

`stemma operations` shows the installed contracts.
`stemma schema --offline --output-file stemma.schema.json` includes their schemas for your editor.

Woodstar's experimental plugin is an external consumer of this interface. Its
Munki-compatible publishing belongs to that plugin; Stemma does not register it as
a built-in or require it to run a catalog.

`trusted: true` authorises execution with your privileges. Workspaces isolate
working files but are not a security sandbox. Review plugin code and lock changes
as executable code. See [writing plugins](writing-plugins.md) for the SDK and wire
contract.

## Reconciliation order

A destination may name, while validating a document, the resources the same
connection has to reconcile first, such as the software an Intune app depends on.
Stemma applies those peers before the document and fails validation on a cycle.
A named resource the run does not reconcile is left to the destination, which
finds its publication in its own state. How native fields refer to other software
remains each destination's contract.

## Icons

A resource that declares an icon supplies the committed PNG as the `icon` artifact in
reconciliation `inputs`. Publish those exact bytes: create a missing icon, replace
one whose content differs and keep the published icon when the input is
absent. Icon publication must not trigger installer uploads. The destination owns
API calls, content storage and icon presence detection.
