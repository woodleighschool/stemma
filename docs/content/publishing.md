# Publishing software

A destination connection belongs to the Project. Each software document supplies
destination settings under that connection's alias. An alias can be any useful name;
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

Stemma derives supported fields from the selected artifact. Values you set take
precedence. For fields you manage:

- Omitted fields are left unchanged unless derivation owns them.
- Supported null values clear a field.
- Supplied lists replace the complete collection, including an empty list.

A field you set or derivation owns is managed. Missing derived values clear the
field, or fail publication if the destination requires a value. Other fields
remain unchanged.
For example, omitting Intune `assignments` preserves existing assignments. It does
not assert that an app is unassigned. Likewise, removing a field from YAML is not
an instruction to clear its remote value unless derivation owns it. To publish a
value other than the derived one, set it.

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

The provider writes pkginfo, installers, declared icons and catalog indexes as
`munkiimport` and `makecatalogs` would: a new item becomes
`pkgsinfo/<name>-<version>.plist` and `pkgs/<installer filename>`, numbered when
another item holds the name, and catalog entries omit `notes` and private keys. An
item already in the repository keeps its paths. You do not need an intermediate
pkginfo-rendering document. `version` is the prepared installer's managed version:
the selected application's, otherwise the Distribution product version or the
version shared by its components. Application evidence supplies detection and DMG
copy details where applicable. A PKG's static PackageInfo and Distribution
declarations supply receipts, installed size and restart requirement; installer
scripts are never evaluated. Receipts or copied items also supply the removal
method, and an item with a method is uninstallable unless `uninstallable` is set.
Explicit `installs`, `receipts`, `installcheck_script` and other supported native
fields allow more specific behaviour.

`minimum_os_version` is the software's effective minimum macOS: the latest of the
installer's requirement, the selected application's and the software's
[`minimum_os`](mac-software.md#minimum-macos). Set `minimum_os` to raise it; pkginfo
cannot declare it.

Munki's `supported_architectures` is an optional pkginfo restriction. There is no
core `spec.arch`: download selection, installation eligibility and runner
architecture are separate decisions. Items sharing a name and version are
architecture variants; set the restriction on each so a document selects its own.

A [declared icon](mac-software.md#icons) arrives as an artifact input. Munki
publishes it by content hash and Intune through its app icon field, both
independently of installer uploads. Changed bytes replace the published icon and a
missing one is added to an existing publication. Icons already in the Munki
repository can be referenced with `pkginfo.icon_name` instead; that path is relative
to the repository's `icons/` directory, not to the catalog.

For source-free script items, see [nopkg](mac-software.md#publish-without-an-installer).
Native endpoint behaviour is described in the
[Munki documentation](https://github.com/munki/munki/wiki).

## Publication relationships

Munki `requires` and `update_for` accept native strings or explicit resource
references. Built-in Munki and the Woodstar destination use the same syntax:

```yaml
pkginfo:
  requires:
    - Some Manually Created Munki Item
    - External Item--1.2.3
    - resource:
        kind: MacSoftware
        name: google-chrome
    - resource:
        kind: MacSoftware
        name: example
      version: "1.2.3"
```

A string is always a literal native Munki reference, including Munki's version
syntax. It never selects a Stemma resource. A `resource` reference resolves the exact
`apiVersion/kind/name`, with `apiVersion` defaulting to `stemma/v1alpha1`. The peer
must declare the same named destination connection. The destination translates its
metadata into the native name: `google-chrome` may publish as `Google Chrome` through
`pkginfo.name`. An optional relationship-level `version` selects a package version.

Selected peers reconcile first. Unselected peers contribute declared metadata to
locate an existing publication without being added to the run. Unknown references,
wrong connections and cycles fail validation; they never fall back to native names.
These publication relationships are separate from immutable
[resource-output dependencies](catalogs.md#two-dependency-graphs).

## Intune

The [Windows guide](windows-software.md) includes a complete connection and Win32
examples. Authentication uses client credentials or an explicit access token.
Graph application permission `DeviceManagementApps.ReadWrite.All` is needed to
apply app changes; read-only planning needs app read access.

Metadata names Intune concepts in Stemma's own fields, such as `display_name`,
`install_experience.run_as` and `detection`; the schema lists them all. The
software kind fixes the platform, so `type` follows the installer:

| `type`  | Software        | Content                                                     |
| ------- | --------------- | ----------------------------------------------------------- |
| `win32` | WindowsSoftware | MSI, EXE or setup tree, prepared internally as `.intunewin` |
| `dmg`   | MacSoftware     | A Mac DMG                                                   |
| `pkg`   | MacSoftware     | A Mac PKG                                                   |
| `lob`   | MacSoftware     | A signed flat PKG, as a line-of-business app                |

Assignments target an Entra group by object ID, an excluded group, all devices or
all users:

```yaml
assignments:
  - intent: required
    group: 11111111-2222-3333-4444-555555555555
  - intent: available
    all_users: true
```

An included target can carry an assignment filter by ID and, for a Win32 app,
its end-user notifications. A setting the assignment omits keeps its value, and
`filter: null` removes a filter:

```yaml
assignments:
  - intent: required
    all_devices: true
    filter:
      id: 66666666-7777-8888-9999-000000000000
      mode: include
    notifications: hide_all
```

Detection uses application bundle identifiers and versions, with the selected
application first. PKG apps can detect applications outside `/Applications`, and
use package receipt identifiers and versions when a package installs no
applications, as a payloadless package does. Line-of-business apps require
applications under `/Applications`. Set `included_apps` to replace the derived list
when static inspection cannot determine the installed applications or an installer
chooses them conditionally. A PKG app accepts receipts there too, so it can be
detected by its package even when it installs an application. `display_name`,
`description` and `publisher` are always set; the artifact never supplies them.

A PKG can also publish as a line-of-business app, so that is the one `type` to
declare. Stemma checks what Intune requires before uploading it: a flat PKG with
a payload that installs an application under `/Applications`, at most 2 GiB,
with a verified Developer ID Installer signature, so the software needs
`signature.signer`. `install_as_managed` also needs one component that installs
one application under `/Applications`.

The minimum OS is the setting for the release of the software's effective
[minimum macOS](mac-software.md#minimum-macos): its major version, or major and
minor for 10.x, so 14.2 selects macOS 14 and the plan shows that mapping. A
release Intune has no setting for fails publication. macOS scripts are
unsupported.

### Intune relationships

Dependencies and supersedence connect compatible Win32 apps on the same Project
connection:

```yaml
destinations:
  intune:
    dependencies:
      - resource:
          kind: WindowsSoftware
          name: vc-runtime
        auto_install: true
    supersedes:
      - resource:
          kind: WindowsSoftware
          name: legacy-client
        uninstall_previous: true
```

Resource references resolve to the app carrying that resource's canonical identity
marker, or to the `app_id` it declares for this connection. An external app uses
`app_id` directly instead of `resource`, with the same relationship policy:

```yaml
dependencies:
  - app_id: 11111111-2222-3333-4444-555555555555
    auto_install: true
```

Exactly one of `resource` and `app_id` is required. Cycles, unpublished references
and incompatible app types fail. The provider permits up to 99 declared
dependencies and 9 supersedence targets, subject to the service's graph limits.

An ordinary update publishes new content to the same app ID. Supersedence relates
separate apps explicitly; it does not create a new app for every release or retire
the old app automatically. Omitted relationship categories are left unchanged;
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
        client_id: "{{ env.JAMF_CLIENT_ID }}"
        client_secret: "{{ env.JAMF_CLIENT_SECRET }}"
```

Jamf publishes PKG artifacts: a vendor package, one selected with `package_path`
or a [BuildMacPkg](building-packages.md) output. Jamf installs a DMG by copying its
contents onto the startup disk, so an application DMG is not a Jamf package.

Each installer filename is one Jamf package record, keeping its native filename;
`display_name` defaults to the filename. Changed bytes under the same filename
upload into the same record. Upload requires a Jamf distribution configuration
supporting the package upload API. `package_id` pins an existing package by its
ID instead.

To associate a package with an existing patch title and maintain a policy:

```yaml
destinations:
  jamf:
    category: Productivity
    retention:
      keep: 1
    patch:
      title: Example
      policy:
        name: Example updates
        enabled: false
        scope:
          all_computers: false
          computer_groups:
            - Staff Macs
```

Categories, patch titles, policies and scope objects are named exactly as in
Jamf. Each name must match exactly one object, or publication fails before
anything is written. The version the title deploys is the software's managed
version: the selected application's under `version_key`, otherwise the
installer's. The title must already define that version; a missing one fails and
lists the title's recent definitions. Stemma does not create definitions from the
package.

The policy is found by its name under the title, which defaults to the resource's
name, and keeps its ID as its target version changes. A title's link to a package
protects it during cleanup, so retention reaches a patch-managed package only
once no version links to it. Stemma manages no other Jamf policies.

## Identity and retention

Destinations identify publications from native keys or markers. Planning and
applying need no local publication state.

| Destination | Identity                                                                |
| ----------- | ----------------------------------------------------------------------- |
| Intune      | A marker line Stemma keeps in the app's notes, or `app_id` when set     |
| Jamf        | A marker line in the package's notes plus its filename, or `package_id` |
| Munki       | The item's name and version, and its architectures when set             |

Munki reconciles an existing item with the same identity. Intune and Jamf require
the marker or an explicit `app_id` or `package_id`; display names are not unique.
The marker occupies the final line of the notes and preserves the remaining text.

Changed bytes at the same version replace the existing content. After an
interruption, the next run finds the remote object and uploads content if needed.
Duplicate identities fail as ambiguous.
`retention.keep: N` retains the current publication and the N−1 newest others the
destination holds for the same software, including ones published before Stemma,
plus anything still needed by native references. Protected publications do not
consume those N slots.

| Destination | Family and order                                              | Cleanup                                                            |
| ----------- | ------------------------------------------------------------- | ------------------------------------------------------------------ |
| Intune      | The app's committed content versions, by version number       | Inactive content versions and abandoned uploads; never app objects |
| Jamf        | Packages carrying the identity marker, by package ID          | Unreferenced package records and their content                     |
| Munki       | Items sharing the name and declared architectures, by version | Pkginfo, catalog entries, unreferenced installers                  |

Cleanup follows successful publication and intended reference updates. Incomplete
reference reads block cleanup. Munki also preserves referenced items whose
catalogs or installation requirements differ from the current publication.

Keeping older content is not a rollback guarantee. Intune's current commands and
detection are not versioned with historical content; uninstall files must remain
available through the current deployment.
