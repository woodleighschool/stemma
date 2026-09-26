# Mac software

Use `MacSoftware` for an existing installer or application. Stemma selects the
application, inspects its metadata and prepares content suitable for publication.
Use [BuildMacPkg](building-packages.md) when you need to construct a custom payload.

## Start with the vendor's installer

| Source                                     | What Stemma prepares                                                          |
| ------------------------------------------ | ----------------------------------------------------------------------------- |
| PKG                                        | The original vendor package                                                   |
| DMG containing an application              | The original DMG, with the selected application described for the destination |
| DMG or archive containing an installer PKG | The selected nested package, preserving its bytes                             |
| Archive or tree containing an application  | A new DMG holding the selected application                                    |

For an application in a DMG:

```yaml
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: example-app
spec:
  source:
    path: Assets/Example.dmg
  application:
    path: Example.app
  destinations:
    munki:
      pkginfo:
        description: Example application.
```

This assumes an `Assets/Example.dmg` beside the document and a Project connection
named `munki`. Substitute your application's actual path.

## Publish an application from an archive

A vendor's PKG or DMG keeps its original bytes. An application that arrives on its
own, in a ZIP or TAR archive or as a committed `.app` tree, is placed at the root
of a new DMG. One document covers a GitHub release:

```yaml
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: example-app
spec:
  source:
    resolver: github
    repository: company/application
    asset: Example-*.zip
  signature:
    signer: apple:developer-id:ABCDE12345
  destinations:
    munki:
      pkginfo:
        description: Example application.
```

The image holds the application and nothing else: its files, permission bits and
symlinks, dated from the locked source, so the same source prepares the same bytes
on any runner. Munki copies the application with `copy_from_dmg` and Intune
publishes the image as `type: dmg`. The application installs to
`/Applications/<name>.app` unless `application.installed_path` says otherwise.
[Signature](#signature) verifies the application; the image is a container and
carries no signature of its own.

## Select an application once

Inspection lists every application and package in the installer. If it finds one
application, it can be selected automatically. If it finds several, select by
`application.path` or `application.bundle_id`:

```yaml
application:
  bundle_id: com.google.Chrome
```

The selection supplies coherent version, bundle and supported icon evidence to
destinations. A helper application should not accidentally become the version or
detection source for the main application. The other applications stay in the
published facts.

Archive paths and endpoint paths are different:

```yaml
application:
  path: Release/Example.app
  installed_path: /Applications/Example.app
  version_key: CFBundleVersion
```

`path` locates the application inside the input. `installed_path` describes its
location on a managed Mac; it is not a path Stemma reads on the runner. The default
version key is `CFBundleShortVersionString`, unless that value is missing or does
not begin with a digit and `CFBundleVersion` is present. Select `CFBundleVersion`
when the publisher's build number is the version you need to manage.

For an existing PKG, Stemma does not rewrite its payload to match a declared
endpoint path. Keep that path consistent with the vendor installer.

## Select a nested installer

A driver download such as Wacom can contain a PKG inside a DMG. When the image or
archive holds more than one application or package, select the installer by its
archive-relative path:

```yaml
spec:
  source:
    path: Assets/WacomDriver.dmg
  package_path: Install Wacom Tablet.pkg
  destinations:
    munki:
      pkginfo:
        description: Wacom tablet driver.
```

Use the name present in your download. `package_path` also accepts a glob, but it
must identify exactly one package. This selects and extracts the vendor installer;
it does not reconstruct its payload or run its scripts. `application` then selects
within the extracted package, as it does when an image holds a single package and
no application matches.

Some packages install a staging helper which later downloads the real application.
Their contents cannot prove the eventual installed application. Leave
`application` unset and set the destination's detection fields explicitly.

## Minimum macOS

Destinations receive one minimum macOS: `minimum_os` when it is set, otherwise the
latest of the installer's requirement and the selected application's
`LSMinimumSystemVersion`:

```yaml
spec:
  minimum_os: "14.0"
```

Changing `minimum_os` reuses prepared installers. Each destination maps the value
to its own field, as described in [publishing](publishing.md).

## Publish without an installer

Munki `nopkg` items need no fake source or package:

```yaml
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: example-setting
spec:
  destinations:
    munki:
      pkginfo:
        installer_type: nopkg
        version: "1.0"
        description: Set an example machine preference.
        installcheck_script: |
          #!/bin/sh
          value=$(/usr/bin/defaults read /Library/Preferences/org.example.settings Enabled 2>/dev/null)
          [ "$value" = "1" ] && exit 1
          exit 0
        postinstall_script: |
          #!/bin/sh
          /usr/bin/defaults write /Library/Preferences/org.example.settings Enabled -bool true
```

Munki's install check exits zero when installation is needed. These scripts run on
the endpoint through Munki, never during preparation. Other destinations must
explicitly support a source-free deployment mode; accepting PKG files alone does
not imply that support.

## Signature

Require the published artifact to carry a complete, valid Developer ID signature
from an expected team:

```yaml
signature:
  signer: apple:developer-id:UBF8T346G9 # Microsoft Corporation
```

`stemma signature MacSoftware/<name>` derives the value from the acquired source,
verifying it first, and prints this fragment to paste. The comment is display
information only. The vendor PKG is verified when that is what Stemma publishes
(a PKG source or `package_path`). A vendor DMG has every application verified,
except those inside another application, which are covered by its signature; an
application that goes into a new DMG is verified on its own. Verification covers
every architecture's code, Info.plist, the resource envelope, symlinks and nested
code by its exact recorded cdhash, chained to Apple's roots at the signature's
trusted timestamp. A different team on any of them fails preparation until the
document is updated. Notarisation and Gatekeeper policy are not assessed. See
[signature limits](limitations.md#signatures).

## Icons

An icon is a committed catalog asset, not something a run derives. Declare it by
name:

```yaml
spec:
  icon: microsoft-word
```

`icon: microsoft-word` means `icons/microsoft-word.png` at the project root. Every
destination publishes those exact bytes on any runner, and a changed file is
ordinary drift that the next run replaces. Without `icon`, published artwork is
left unchanged. A declared icon without its file fails `validate`,
`plan` and `apply`.

Create the asset from the software itself:

```sh
stemma icon MacSoftware/microsoft-word
stemma icon
```

`stemma icon` prepares the locked source like any other run and takes the
application that [selection](#select-an-application-once) identifies. Disk images,
archives and vendor packages all work; a package holding several applications needs
`application.bundle_id` or `application.path` first. Without selectors it creates
every declared icon that has no file yet and existing files stay unchanged, so a run
across the catalog is safe. `--force` replaces them.

Both presentations select the file named by `CFBundleIconFile`, adding `.icns`
when the declaration omits an extension.

`glassy`, the default on a Mac, uses the macOS renderer at 512 pixels; `--size`
changes the edge. It stages the declared icon, `Info.plist`, `PkgInfo` when present,
and `Assets.car` when `CFBundleIconName` declares an asset-backed icon. The renderer
selects the artwork from those resources. A small native Mach-O marker prevents
missing-executable badges; vendor executables and their symlink targets are never
extracted or run. Applications with only a named asset icon do not require an ICNS
fallback. Missing artwork reports `no artwork`.

`raw`, the default elsewhere, extracts only the declared icon and writes its largest
supported PNG entry unchanged. Legacy-only ICNS encodings report `no artwork`.
`--presentation` selects either presentation explicitly. A package's payload is
still read through once to reach the selected files, which requires decompressing it.

`icons/` holds plain PNG files. Software without an application, such as a
script-only item or a driver package, uses artwork you commit yourself: any square
PNG between 128 and 1024 pixels, up to 1 MiB. Resources share an asset by naming
it, so a `WindowsSoftware` document can publish the icon created from its macOS
counterpart, or create its own.
