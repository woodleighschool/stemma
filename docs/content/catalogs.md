# Writing a catalog

A catalog is a Git repository with a `stemma.yaml` Project and imported resource
documents. The Project owns composition, named destination connections, plugins
and the [source-control integration](reconcile.md) of its repository. Resource
documents describe individual builds or software items.

## Files and identities

Keep related definitions and their assets together. For example:

```text
stemma.yaml
stemma.lock.yaml
icons/
  chrome.png
software/
  chrome.yaml
  fonts/
    buildmacpkg.yaml
    macsoftware.yaml
    Fonts/
```

Names and directories are your choice. `spec.imports` accepts paths and globs such
as `software/**/*.yaml`; every pattern must match. Asset paths are relative to the
document containing the declaration and must remain inside the project. `icons/` is
the one fixed name: a resource's `icon: chrome` publishes `icons/chrome.png`. See
[icons](mac-software.md#icons).

Each document has a literal `apiVersion`, `kind`, `metadata.name` and `spec`.
Kinds are case-sensitive: use `MacSoftware`, not `macsoftware`. Lowercase filenames
such as `macsoftware.yaml` are just a convention.

A family can share one file using YAML document separators:

```yaml
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: chrome
spec:
  source:
    url: https://dl.google.com/dl/chrome/mac/universal/stable/gcem/GoogleChrome.pkg
  application:
    bundle_id: com.google.Chrome
  destinations:
    munki:
      pkginfo:
        description: Google Chrome.
---
apiVersion: stemma/v1alpha1
kind: WindowsSoftware
metadata:
  name: chrome
spec:
  extends: windows-win32
  source:
    url: https://dl.google.com/dl/chrome/install/googlechromestandaloneenterprise64.msi
  destinations:
    intune:
      description: Google Chrome.
```

This uses the `munki` connection from [getting started](getting-started.md) and the
`intune` connection and `windows-win32` component from [Windows software](windows-software.md).

A resource's identity is `apiVersion/kind/name`, independent of its filename or
native destination name. Mac and Windows documents can share a name, including on
the same connection. Each destination owns its native publication identity.

## Two dependency graphs

Resource-output dependencies, such as `source.resource`, consume another resource's
immutable output. They order preparation and include the producer when selecting a
consumer. `output` selects the artifact and defaults to `installer`.

Publication relationships, such as Munki `requires` / `update_for` and Intune
`dependencies` / `supersedes`, refer to another resource's publication on the same
named destination connection. They order selected publications without selecting or
mutating unselected peers. An unselected peer contributes its declared destination
metadata so the destination can find an existing publication.

Both use an explicit `kind` and `name`; `apiVersion` defaults to
`stemma/v1alpha1`. Publication references have no `output`. Missing resources,
missing publications on the named connection and publication cycles are errors.
See [publication relationships](publishing.md#publication-relationships).

## Choose a kind

| Kind              | Use it for                                                                                  |
| ----------------- | ------------------------------------------------------------------------------------------- |
| `MacSoftware`     | A vendor installer or application, or a source-free native deployment such as Munki `nopkg` |
| `WindowsSoftware` | An existing vendor MSI, EXE or setup directory and its destination settings                 |
| `BuildMacPkg`     | Constructing a package from declared files, directories and installer scripts               |

A build reference selects an artifact, not an installed application dependency.
For example, a `MacSoftware` source can reference the `installer` output of a
`BuildMacPkg`. Intune dependencies instead connect native app objects. See
[building packages](building-packages.md) and [publishing](publishing.md).

## Share defaults when they help

Project components contain reusable spec values:

```yaml
spec:
  components:
    mac-software:
      destinations:
        munki:
          pkginfo:
            catalogs:
              - testing
          retention:
            keep: 1
```

A resource uses `spec.extends: mac-software`. Maps merge recursively; supplied
lists and explicit nulls replace inherited values. A component is optional. Keep
values in the individual document when sharing them would obscure the app's intent.

Inspect the result with `stemma validate --resolved`. Treat resolved output as
configuration: it may contain values supplied through your environment.

## Suspend a resource

Some resources need files the repository does not carry, such as licensed fonts
or a vendor package kept out of version control. Declare them and set
`suspend: true` beside `metadata`:

```yaml
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: fonts
suspend: true
spec:
  inputs:
    fonts:
      path: Fonts
  package:
    identifier: edu.example.fonts
    version: "1.0"
  payload:
    /Library/Fonts:
      $input: fonts
```

A suspended resource keeps its reviewed lock entries and is still checked by
`stemma validate`, but runs without selectors and [reconciliation](reconcile.md)
skip it. On a machine
holding the files, `stemma apply MacSoftware/fonts` runs it together with the
builds it references. A resource that is not suspended cannot consume a suspended
resource's outputs: suspend both and select the consumer.

## Expressions and editor support

Use [expressions](expressions.md) for environment values and inspected metadata:
`client_secret: "{{ env.INTUNE_CLIENT_SECRET }}"`. Stemma reads the process
environment; it does not source `.env`. Expressions can fill a whole value or
interpolate into text. Document identities, mapping keys and resource references
remain literal.

The [default schema](https://woodleighschool.github.io/stemma/stemma.schema.json)
describes built-in operations and accepts arbitrary destination names. Generate a
catalog-specific schema from the local registry, including installed plugins
and the Project's named destinations:

```sh
stemma schema --offline --output-file stemma.schema.json
```

Track the generated file in the catalog and add a YAML language-server modeline
using its raw repository URL:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/OWNER/CATALOG/main/stemma.schema.json
```

Regenerate it when plugin declarations or destination names change.
`--output-file` is required; use `--output-file -` to print JSON to stdout. Schema generation does not need destination
credentials or acquire software, but OCI plugin bundles must already be cached
when using `--offline`. Go validation also checks semantic rules that are broader
in the editor schema, such as valid Microsoft product/channel combinations.
