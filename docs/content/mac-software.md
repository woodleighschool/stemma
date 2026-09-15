# Mac software

Use `MacSoftware` for an existing installer or application. Stemma selects the
application, inspects its metadata and prepares content suitable for publication.
Use [BuildMacPkg](building-packages.md) when you need to construct a custom payload.

## Start with the vendor's installer

| Source                          | What Stemma prepares                                                          |
| ------------------------------- | ----------------------------------------------------------------------------- |
| PKG                             | The original vendor package                                                   |
| DMG containing an application   | The original DMG, with the selected application described for the destination |
| ZIP containing an application   | An unsigned PKG containing the selected application                           |
| DMG containing an installer PKG | The selected nested package, preserving its bytes                             |

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
named `munki`. Substitute your application's actual path. For a ZIP application,
change the source to the ZIP; no separate build document is needed.

## Select an application once

If inspection finds one application, it can be selected automatically. If it finds
several, select by `application.path` or `application.bundle_id`:

```yaml
application:
  bundle_id: com.google.Chrome
```

The selection supplies coherent version, bundle and supported icon evidence to
destinations. A helper application should not accidentally become the version or
detection source for the main application.

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

For an existing PKG, Stemma does not rewrite its payload to implement an authored
endpoint path. Keep that path consistent with the vendor installer.

## Select a nested installer

A driver download such as Wacom can contain a PKG inside a DMG. When there are
multiple plausible payloads, select the installer by its archive-relative path:

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
it does not reconstruct its payload or run its scripts.

Some packages install a staging helper which later downloads the real application.
Their contents cannot prove the eventual installed application. Leave
`application` unset and author the destination's detection behaviour explicitly.

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
(a PKG source or `package_path`); otherwise the selected application is:
every architecture's code, Info.plist, the resource envelope, symlinks and nested
code by its exact recorded cdhash, chained to Apple's roots at the signature's
trusted timestamp. A different team fails preparation until the document is
updated. Notarisation and Gatekeeper policy are not assessed. See
[signature limits](limitations.md#signatures).

## Application icons

A selected application produces an immutable `icon` PNG artifact whenever supported.
macOS runners use the native system renderer for the best current macOS appearance,
including system styling. Linux and Windows runners use supported plist, PNG and
ICNS resources from the application bundle, so ordinary CI can bootstrap a catalog
with icons without per-application configuration.

Native rendering is an optional enhancement. If it is unavailable, preparation
uses a supported portable icon or omits the output. Unsupported applications and
installers may legitimately have no icon. A vendor PKG with application metadata
but no selected application directory is not rendered as an application icon.

Destinations normally create an icon only when one is missing, even if the software
object already exists. Existing artwork is retained across normal runs and software
updates, so a portable runner does not replace a native icon. To improve the catalog's
artwork intentionally, run on a current Mac:

```sh
stemma apply --refresh-icons
stemma apply MacSoftware/firefox --refresh-icons
```

Use `stemma plan --refresh-icons` to preview publication changes. Refresh derives
icons again on this runner and permits destinations to replace their existing icons.
Installer preparation remains cached; icon changes do not change installer bytes,
versions or content identity. Native and portable icon preparations have separate
cache entries. Munki keeps immutable icon objects and updates their references.
External destinations must implement the icon reconciliation contract described in
[plugins](plugins.md).
