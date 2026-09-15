# Publishing software

A destination connection belongs to the Project. Each software document supplies
native settings under that connection's alias. An alias can be any useful name;
its `operation` selects the implementation.

Use `stemma plan` to read a destination and see proposed changes. `stemma apply`
re-observes and reconciles it. Both require prepared, reviewed input locks. Stemma
reconciles destinations independently; publication to several services is not one
transaction.

Mac installer outputs use `<name>-<version>.<ext>`, with a digest prefix when no
version is available. `BuildMacPkg` can override the filename. Intune Win32
envelopes follow the same naming convention while preserving setup filenames
inside the payload. Input filenames and publication names are independent.

## Native fields and derived values

Stemma derives supported fields from the selected artifact. Explicit authored
values take precedence. For fields you manage:

- Omitted fields remain unmanaged unless supplied by derivation.
- Supported null values clear a field.
- Supplied lists replace the complete collection, including an empty list.

For example, omitting Intune `assignments` preserves existing assignments. It does
not assert that an app is unassigned. Likewise, removing a field from YAML is not
an instruction to clear its remote value. `unmanaged` can relinquish supported
derived fields; the destination schema lists the available fields.

## Munki

The built-in `munki` destination writes a filesystem repository:

```yaml
spec:
  destinations:
    munki:
      operation: munki
      config:
        path: /srv/munki
```

Resource settings use native pkginfo fields:

```yaml
destinations:
  munki:
    pkginfo:
      catalogs:
        - testing
      description: Example application.
      category: Productivity
    retention:
      keep: 1
```

The provider writes pkginfo, installer objects, supported derived icons and catalog
indexes. You do not need an intermediate pkginfo-rendering document. Application
evidence supplies detection and DMG copy details where applicable. A PKG's static
PackageInfo and Distribution declarations supply receipts, installed size, minimum
macOS version and restart requirement; installer scripts are never evaluated.
Receipts or copied items also make the item uninstallable unless removal is
authored. Without a selected application, a PKG's version is its Distribution
product version or the version shared by its components. Explicit `installs`,
`receipts`, `installcheck_script` and other supported native fields allow more
specific behaviour.

Munki's `supported_architectures` is an optional pkginfo restriction. There is no
core `spec.arch`: download selection, installation eligibility and runner
architecture are separate decisions.

Custom icons already in the Munki repository can be referenced with
`pkginfo.icon_name`. That path is relative to the repository's `icons/` directory;
it is not an instruction to read an arbitrary catalog file. Automatic application
icons arrive as artifact inputs. Munki publishes them by content hash and retains
existing artwork unless `--refresh-icons` is requested. A missing icon can be added
to an existing software publication. Intune reconciles the same artifact through
its app icon field, independently of installer uploads. External destinations receive
the same refresh intent and implement their native icon handling.

For source-free script items, see [nopkg](mac-software.md#publish-without-an-installer).
Native endpoint behaviour is described in the
[Munki documentation](https://github.com/munki/munki/wiki).

## Intune

The [Windows guide](windows-software.md) includes a complete connection and Win32
examples. Authentication uses client credentials or an explicit access token.
Graph application permission `DeviceManagementApps.ReadWrite.All` is needed to
apply app changes; read-only planning needs app read access.

The provider supports these native app types:

| `type`  | Content                                                     |
| ------- | ----------------------------------------------------------- |
| `win32` | MSI, EXE or setup tree, prepared internally as `.intunewin` |
| `dmg`   | A Mac DMG, uploaded as a `macOSDmgApp`                      |
| `pkg`   | A Mac PKG, uploaded as a `macOSPkgApp`                      |

Mac app metadata uses the current beta API. Native `macOSLobApp`, macOS scripts and
fields outside the generated schema are unsupported. The selected application can
supply bundle identity and versions; review the native `includedApps`,
`minimumSupportedOperatingSystem` and detection choices before publication.

### Intune relationships

Dependencies and supersedence connect compatible Win32 apps on the same Project
connection:

```yaml
destinations:
  intune:
    type: win32
    dependencies:
      - software: vc-runtime
        auto_install: true
    supersedes:
      - software: legacy-client
        uninstall_previous: true
```

References resolve stable Stemma names through durable app bindings, not display
names or MSI ProductCodes. Publish the referenced items first. Cycles, missing
bindings and incompatible app types fail. The provider permits up to 99 authored
dependencies and 9 supersedence targets, subject to the service's graph limits.

An ordinary update publishes new content to the same app ID. Supersedence relates
separate apps explicitly; it does not create a new app for every release or retire
the old app automatically. Omitted relationship categories remain unmanaged;
`dependencies: []` clears that outgoing category.

An unassigned app may still install as a dependency of an assigned parent. Review
incoming relationships as well as assignments when testing in a live tenant.

## Jamf

Connect using a Jamf API client with package privileges:

```yaml
spec:
  destinations:
    jamf:
      operation: jamf
      config:
        url: https://example.jamfcloud.com
        client_id: ${JAMF_CLIENT_ID}
        client_secret: ${JAMF_CLIENT_SECRET}
```

Jamf receives an immutable package ID for each distinct artifact. Package display
names default to the installer filename. Remote filenames include ownership and
content markers. Upload requires a Jamf distribution configuration supporting the
package upload API.

To associate a package with an existing patch title and maintain a policy:

```yaml
destinations:
  jamf:
    retention:
      keep: 1
    patch:
      title_configuration_id: "42"
      version:
        $fact: macos.application.app.version
      policy:
        name: Example updates
        enabled: false
        scope:
          all_computers: false
          computers: []
          computer_groups: []
```

Use an existing title ID and an exact version known to that title. This example
uses the selected Mac application's version. The title's definition must already
contain that version; Stemma does not create the definition from the package.

The bound policy keeps its ID as its target version changes. A disabled or unscoped
policy still protects the version it references during cleanup. Package upload and
patch policy management do not provide a general Jamf policy-authoring interface.

## Identity and retention

Keep `.stemma/state` durable, including when moving a catalog to another runner.
It records native IDs, owned fields, payload identities and successful publication
order. A similarly named remote object is not proof of ownership. Explicit
`app_id` or `package_id` adoption must agree with an existing binding and the
provider's content checks. Remote marker recovery cannot recreate lost historical
publication order or association ownership.

`retention.keep: N` retains the current payload and the N−1 most recently
successfully published distinct payloads, plus anything still needed by native
references. Protected older payloads do not consume those N slots.

| Destination | Cleanup                                                                  |
| ----------- | ------------------------------------------------------------------------ |
| Intune      | Eligible inactive content versions within the bound app; not app objects |
| Jamf        | Eligible owned obsolete title associations, then unreferenced packages   |
| Munki       | Eligible package-version records, then unreferenced installer objects    |

Cleanup follows successful publication and intended reference updates. Metadata
edits neither re-upload unchanged content nor advance publication order. Unknown
ownership, unknown order, changed owned associations or incomplete reference reads
block destructive cleanup.

Keeping older content is not a rollback guarantee. Intune's current commands and
detection are not versioned with historical content; uninstall files must remain
available through the current deployment.
