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
software/
  chrome.yaml
  fonts/
    buildmacpkg.yaml
    macsoftware.yaml
    Fonts/
```

Names and directories are your choice. `spec.imports` accepts paths and globs such
as `software/**/*.yaml`; every pattern must match. Asset paths are relative to the
document containing the declaration and must remain inside the project.

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

`metadata.name` is a stable identity, independent of the filename or display name.
Renaming a file does not rename a destination binding. Mac and Windows documents
can share a name, but two resources cannot publish the same name to the same
connection. Use separate connection aliases when that distinction is needed.

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

A suspended resource still validates and keeps its reviewed lock entries, but runs
without selectors and [reconciliation](reconcile.md) leave it alone. On a machine
holding the files, `stemma apply MacSoftware/fonts` runs it together with the
builds it references. A resource that is not suspended cannot consume a suspended
resource's outputs: suspend both and select the consumer.

## Environment and editor support

Use a whole-value placeholder such as `client_secret: ${INTUNE_CLIENT_SECRET}` for
credentials. Stemma reads the process environment; it does not source `.env`.
Interpolation inside larger strings and mapping keys is unsupported. Document
identities must remain literal.

The [generated schema](https://github.com/woodleighschool/stemma/blob/main/stemma.schema.json)
describes built-in documents. To include your installed plugins and bind destination
aliases to their schemas:

```sh
stemma schema --project --offline > stemma.project.schema.json
```

Point your YAML editor at that file. Schema generation does not need destination
credentials or acquire software, but OCI plugin bundles must already be cached
when using `--offline`.
