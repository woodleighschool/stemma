# MacSoftware

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
  signatures:
    - signer: apple:developer-id:ABCDE12345
  destinations:
    munki:
      pkginfo:
        description: Example application.
```

The image holds the application: its files, permission bits, symlinks and archive
extended attributes, including resource forks. AppleDouble sidecars and PAX
attributes stay associated with their files without being applied to the runner.
Ownership and dates are normalized, so the same source prepares the same bytes
on any runner. Munki copies the application with `copy_from_dmg` and Intune
publishes the image as `type: dmg`. The application installs to
`/Applications/<name>.app` unless `application.installed_path` says otherwise.
[Signature](#signature) verifies the application; the image is a container and
carries no signature of its own.

### Compress the image

`disk_image.compression` controls how a disk image created by Stemma is compressed:

```yaml
spec:
  disk_image:
    compression: lzma
```

| `compression`     | Image size     | Copy time on a Mac | Compatibility        |
| ----------------- | -------------- | ------------------ | -------------------- |
| `lzfse` (default) | 5–6% smaller   | 11–17% faster      | macOS 10.11 or later |
| `zlib`            | baseline       | baseline           | any macOS            |
| `lzma`            | 12–18% smaller | 2.4–4.5x slower    | macOS 10.15 or later |

`lzfse` is the default because it produces a slightly smaller image than `zlib`
while also being faster for Macs to read.

`lzma` produces the smallest images, but takes 3.5–5x as long to build and is
slower to copy files from. It is most useful when image size matters more than
preparation and installation time.

The figures above come from Chrome, Visual Studio Code and Minecraft Education.
Image creation happens once per release, while managed Macs pay the read cost each
time the image is mounted and its application is copied.

For reference, `hdiutil` calls the LZFSE, zlib and LZMA image formats ULFO, UDZO
and ULMO respectively. LZMA also requires roughly four times as much memory while
building.

`disk_image` applies only when Stemma creates the image. Vendor DMGs and PKGs keep
their original bytes, so declaring `disk_image` for one causes preparation to fail.

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

Declare one exact signing expectation for each published signing subject:

```yaml
signatures:
  - signer: apple:developer-id:UBF8T346G9 # Microsoft Corporation
```

An entry without `subject` covers the artifact's only signing subject. For a PKG
that is the outer package signature: component receipts and payload applications
do not prove that the published PKG is signed. For a DMG it is the application
the image holds. An image holding several top-level applications needs an entry
for each, selected by its exact path or bundle identifier:

```yaml
signatures:
  - subject:
      path: Example.app
    signer: apple:developer-id:ABCDE12345
  - subject:
      path: Example Helper.app
    signer: apple:developer-id:ABCDE12345
```

Every shipped top-level app requires an entry when `signatures` is present,
including companion apps. Nested code is covered by its enclosing app's
signature. An app selected from an archive is published alone at the new image's
root, so its entry needs no `subject`.

`stemma signature MacSoftware/<name>` inspects every signing subject and prints a
complete fragment, independently of existing expectations. It needs no placeholder
declaration. Different apps may name different publishers. The comment is display
information only.

A signer names the publisher's Apple team under the scheme of the certificate
that signed the software. `apple:developer-id:<TEAMID>` is a PKG or application
the publisher signed with its own Developer ID. An application from the Mac App
Store is signed by Apple instead, and its signer is `apple:app-store:<TEAMID>`:

```yaml
signatures:
  - signer: apple:app-store:ABCDE12345
```

Apple's certificate names no publisher, so the team is the one the signed
application names. The schemes are separate expectations: an application that
moves between Developer ID and the App Store changes signer.

Unsigned software can be asserted explicitly:

```yaml
signatures:
  - unsigned: true
```

Each entry requires exactly one of `signer` or `unsigned: true`. Omitting
`signatures` makes no assertion. An unsigned expectation fails when the subject
becomes signed; a signer expectation fails when it becomes unsigned or changes
publisher. Malformed, tampered, ad-hoc and unsupported signatures remain errors in
both preparation and derivation.

A package from [BuildMacPkg](building-packages.md) needs no entry. The builder
never signs, so the package has no publisher to verify and `stemma signature`
reports nothing for it. `unsigned: true` is still checked when declared.

Apple verification covers every architecture's code, Info.plist, the resource
envelope, symlinks and nested code, chained to Apple's roots. A trusted timestamp
fixes the time at which every certificate of a signature must have been valid.
Without one, a PKG's certificates are judged when it is verified, and an
application's are held to no validity period, as on macOS: App Store signatures
carry no timestamp, and Apple's certificate for them expires while the
applications stay installed. Code signed to expire with its certificate needs a
trusted timestamp. Nested code
matches its exact recorded cdhash, or replaces the
sealed code under the Developer ID requirement the app recorded for it: the same
identifier, signed by the same team with a Developer ID Application certificate.
`stemma signature` lists replaced nested code. Nested scripts and data files that
`codesign` signs in extended attributes verify inside vendor DMGs and archives
that carry those attributes. Generated images preserve them; vendor DMGs are
published unchanged. Notarisation and Gatekeeper policy
are not assessed. See [signature limits](limitations.md#signatures).

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
the declared icons that have no file yet and existing files stay unchanged, so a run
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

Output size can change the macOS presentation, not just its resolution. For some
applications, 256 pixels produces the expected app icon while 512 pixels adds a
grey frame around the artwork. If the result looks wrong, compare sizes with
`stemma icon MacSoftware/<name> --force --size 256`. Review the generated image;
a larger size does not necessarily produce a better match to Finder.

`raw`, the default elsewhere, decodes the declared ICNS or raster icon and writes
its largest artwork as PNG. ICNS supports PNG, JPEG 2000, planar colour and indexed
elements. Small artwork is enlarged to 128 pixels; larger or rectangular artwork
is fitted to the asset bounds. Valid PNG assets retain their bytes. Unreadable
artwork reports a decoding error; an asset-catalog-only icon requires `glassy`.
`--presentation` selects either presentation explicitly. A package's payload is
still read through once to reach the selected files, which requires decompressing it.

For a script-only wrapper, declare `icon: example` on the software resource and run
`stemma icon MacSoftware/example --input vendor --path Installer.app`. The command
finds `vendor` through the build dependencies and writes `icons/example.png`.
An input containing multiple applications requires `--path`.

`icons/` holds plain PNG files: square, between 128 and 1024 pixels, up to 1 MiB.
You can also commit an asset directly. Resources share an asset by naming
it, so a `WindowsSoftware` document can publish the icon created from its macOS
counterpart, or create its own.
