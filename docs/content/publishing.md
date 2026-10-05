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

`minimum_os_version` is the software's
[minimum macOS](mac-software.md#minimum-macos); pkginfo cannot declare it.

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

The [Windows apps in Intune guide](intune-windows.md) includes a complete
connection, installation commands and detection examples. Authentication uses client credentials or an explicit access token.
The Entra app needs these Graph application permissions, with admin consent.
Planning needs only the read permission; `DeviceManagementApps.ReadWrite.All`
also grants read access:

| Feature                                      | Read                            | Write                                |
| -------------------------------------------- | ------------------------------- | ------------------------------------ |
| Apps, content and metadata                   | `DeviceManagementApps.Read.All` | `DeviceManagementApps.ReadWrite.All` |
| Assignments (`assignments`)                  | `DeviceManagementApps.Read.All` | `DeviceManagementApps.ReadWrite.All` |
| Categories (`categories`)                    | `DeviceManagementApps.Read.All` | `DeviceManagementApps.ReadWrite.All` |
| Relationships (`dependencies`, `supersedes`) | `DeviceManagementApps.Read.All` | `DeviceManagementApps.ReadWrite.All` |

Groups and assignment filters are referenced by ID, so no directory or filter
permission is needed.

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

`categories` lists the app's Company Portal categories by name and replaces the
app's whole set; `[]` clears them. Names match exactly, including case. Apply
creates a category the tenant doesn't have yet:

```yaml
categories:
  - Productivity
```

For macOS, detection uses the selected application's bundle identifier and version.
Intune reports an installation only when every included app is present, so other
applications an installer carries, such as a bundled updater, are left out. PKG
apps can detect an application outside `/Applications`, and use package receipt
identifiers and versions when a package selects no application, as a payloadless
package does. DMG and line-of-business apps require the application under `/Applications`.
Set `included_apps` to replace the derived list, for example to require every
application of a suite, or when static inspection cannot determine the installed
application or an installer chooses it conditionally. A PKG app accepts receipts
there too, so it can be detected by its package even when it installs an
application. `display_name`,
`description` and `publisher` are always set; the artifact never supplies them.

A PKG can also publish as a line-of-business app, so that is the one `type` to
declare. Stemma checks what Intune requires before uploading it: a flat PKG with
a payload that installs an application under `/Applications`, at most 2 GiB,
with a verified Developer ID Installer signature, so the software needs
a `signatures` entry for the package with the expected `signer`.
An unsigned PKG can use `type: pkg`; it cannot use `type: lob`. `install_as_managed` also needs one component that installs
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

An ordinary update publishes new content to the same app ID, and the app keeps its
earlier content versions. Supersedence relates
separate apps explicitly; it does not create a new app for every release or retire
the old app automatically. Omitted relationship categories are left unchanged;
`dependencies: []` clears that outgoing category.

An unassigned app may still install as a dependency of an assigned parent. Review
incoming relationships as well as assignments when testing in a live tenant.

## Jamf

Connect using a Jamf API client:

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

Each installer filename is one Jamf package record, named after the file.
Changed bytes under the same filename upload into the same record. Upload
requires a Jamf distribution configuration supporting the package upload API.
`package_id` pins an existing package by its ID instead.

Install policies handle first installs, Self Service and enrollment workflows;
the patch policy updates Macs that already have the software.

### Install policies

```yaml
destinations:
  jamf:
    category: Productivity
    policies:
      - name: ALL - Visual Studio Code - ALL
        enabled: true
        category: Applications
        frequency: ongoing
        event: visual_studio_code
        self_service:
          description: Code editor for staff and students.
        scope:
          all_computers: true
      - name: ALL - Visual Studio Code - Staff
        enabled: true
        category: Applications
        triggers: [checkin, enrollment_complete]
        frequency: once_per_computer
        retries: 3
        scope:
          all_computers: false
          computer_groups:
            - Staff Macs
```

Policies are found by exact `name`. Renaming one creates a new policy and leaves
the old one in Jamf. New policies start disabled and unscoped, with inventory
updates enabled. Set `enabled: true` and a scope to deploy them.

Each policy installs the current package. Managing an existing policy replaces
its package list; the plan lists removed packages.

`triggers` selects automatic events; `event` sets a custom trigger for
`jamf policy -event NAME`, and `event: ""` removes it. `frequency` controls how
often the policy runs. Use `ongoing` for Self Service reinstall availability;
`once_per_computer` removes the item after it runs. `retries` retries failures
at check-in and requires `frequency: once_per_computer`. `update_inventory`
controls the inventory update after installation.

`self_service` takes `true`, `false` or settings. The Self Service name defaults
to the selected application's name, then the software name; the category
defaults to the package category; the icon is the software's `icon`. The policy
category also defaults to the package category, and `null` removes either
category. Omitting `icon` leaves existing artwork unchanged. Other omitted
settings, including scripts and restarts, keep their Jamf values.

### Patch policy

```yaml
destinations:
  jamf:
    patch:
      title: Visual Studio Code
      policy:
        enabled: true
        distribution: self_service
        reminder_days: 3
        deadline_days: 7
        grace_minutes: 30
        scope:
          all_computers: true
```

`patch` associates the package with the software's managed version in an existing
patch title: the selected application's version under `version_key`, otherwise
the installer's. Until the title defines that version, the package publishes
alone and a warning lists the title's recent definitions, which also shows when
a title writes versions differently. The link and the policy's target version
follow once the title defines it. The policy's other settings apply meanwhile,
and a new policy waits for the definition. Stemma does not create definitions
from the package.

The policy is found by its name under the title, which defaults to the software
name, and keeps its ID as its target version changes. Like an install policy, a
new one is created disabled and unscoped.

`distribution: automatic` installs updates at check-in. `self_service` offers
them in Self Service with a notification, reminders and the software's icon.
`reminder_days` sets the days between reminders, 1 unless set, and null turns
reminders off. `deadline_days` installs an update automatically once it has been
offered for that many days, and null removes the deadline. `grace_minutes` gives
users time to save their work before the title's apps quit for an update. The
title's definitions supply which apps quit and the minimum macOS.
`patch_unknown: true` also updates Macs whose installed version the title does
not define. `allow_downgrade: true` installs the target version over a newer
one, as when the catalog returns to an earlier release.

Retention preserves every package still linked by a patch title, even with
`keep: 1`. Policies omitted from the declaration remain unchanged.

### Names and privileges

Categories, patch titles and scope objects are named exactly as in Jamf. Each
name must match exactly one object, or publication fails before anything is
written. Scope privileges follow the kind of object named, whether it is a
target, a limitation or an exclusion.

The API client's role needs these privileges for the features a catalog uses.
Planning needs only the read privileges; applying needs both columns. Uploading
Self Service icons needs none.

| Feature                       | Read                                                                                           | Write                                            |
| ----------------------------- | ---------------------------------------------------------------------------------------------- | ------------------------------------------------ |
| Packages                      | `Read Packages`                                                                                | `Create Packages`, `Update Packages`             |
| Category by name (`category`) | `Read Categories`                                                                              |                                                  |
| Install policies (`policies`) | `Read Policies`                                                                                | `Create Policies`, `Update Policies`             |
| Patch title link (`patch`)    | `Read Patch Management Software Titles`                                                        | `Update Patch Management Software Titles`        |
| Patch policy (`patch.policy`) | `Read Patch Policies`                                                                          | `Create Patch Policies`, `Update Patch Policies` |
| Scope naming computers        | `Read Computers`                                                                               |                                                  |
| Scope naming computer groups  | `Read Smart Computer Groups`, `Read Static Computer Groups`                                    |                                                  |
| Scope naming buildings        | `Read Buildings`                                                                               |                                                  |
| Scope naming departments      | `Read Departments`                                                                             |                                                  |
| Scope naming network segments | `Read Network Segments`                                                                        |                                                  |
| Retention (`retention`)       | `Read Policies`, `Read Computer PreStage Enrollments`, `Read Patch Management Software Titles` | `Delete Packages`                                |

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

| Destination | Family and order                                              | Cleanup                                           |
| ----------- | ------------------------------------------------------------- | ------------------------------------------------- |
| Jamf        | Packages carrying the identity marker, by package ID          | Unreferenced package records and their content    |
| Munki       | Items sharing the name and declared architectures, by version | Pkginfo, catalog entries, unreferenced installers |

Cleanup follows successful publication and intended reference updates. Incomplete
reference reads block cleanup. Munki also preserves referenced items whose
catalogs or installation requirements differ from the current publication.

Keeping older content is not a rollback guarantee.
