# WindowsSoftware

Use `WindowsSoftware` for a vendor installer and any files it needs at installation.
It resolves the source, selects the setup entry point and inspects the content
without running it. Its `installer` output is a file or setup directory with
metadata for destinations to use.

## Start with a vendor installer

| Source             | Prepared content                                      |
| ------------------ | ----------------------------------------------------- |
| MSI or EXE         | The original vendor installer                         |
| Directory          | The setup tree and its selected entry point           |
| ZIP or TAR archive | The extracted setup tree and its selected entry point |
| Resource output    | The producing resource's file or tree                 |

Use the shared [source resolvers](sources.md) for downloads, local files and
resource outputs. For example, with the connection from
[Windows apps in Intune](intune-windows.md#configure-intune):

```yaml
apiVersion: stemma/v1alpha1
kind: WindowsSoftware
metadata:
  name: chrome
spec:
  source:
    url: https://dl.google.com/dl/chrome/install/googlechromestandaloneenterprise64.msi
  destinations:
    intune:
      display_name: Google Chrome
      description: Google Chrome.
      publisher: Google LLC
```

`stemma update WindowsSoftware/chrome` locks the source;
`stemma prepare WindowsSoftware/chrome` prepares the installer. Use
`stemma artifact WindowsSoftware/chrome` to obtain a local copy for inspection.
The kind requires destination settings; their fields and publication behaviour
belong to the [destination](publishing.md).

## Select the setup file

A single-file source is its own setup entry point. For a directory or archive,
the only MSI or EXE is selected automatically. When there are several, set
`setup_file` to a setup-relative path or a glob matching exactly one file:

```yaml
source:
  url: https://example.com/downloads/VendorSetup_4.2.zip
setup_file: VendorSetup.exe
```

A resource output can supply its own entry point. An ambiguous directory fails
with the installers it found. An explicit `setup_file` may select another file,
such as a wrapper script; the destination's commands and detection must describe
what that entry point installs.

## Include accompanying files

`content.files` adds inputs at relative paths in the setup directory:

```yaml
source:
  path: Assets/Vendor.msi
content:
  files:
    Organisation.mst:
      path: Assets/Organisation.mst
```

Each value uses the shared input resolvers and can supply a file or tree.
The vendor installer remains the entry point unless `setup_file` selects another.
A destination decides how to use these files; see the
[Intune transform example](intune-windows.md#accompanying-files).

## Select the managed version

The selected MSI supplies ProductCode, ProductVersion and other MSI facts.
ProductVersion is the managed version by default. When the installed product uses
a different version, `version_file` selects one versioned file from the setup
MSI's File table:

```yaml
version_file: Zoom.exe
```

This changes the managed version while keeping the MSI's ProductVersion in its
inspection facts. `version_file` requires an MSI setup file. Other installers can
retain a version supplied with their source artifact; selecting a different entry
point clears that source version.

Installation commands and installed-product detection are destination settings.
See [Intune detection](intune-windows.md#choose-installed-product-evidence) for how
MSI facts and the managed version become detection rules.

## Icon

`spec.icon` names a catalog asset exactly as it does for
[macOS software](mac-software.md#icons): `icon: microsoft-word` publishes
`icons/microsoft-word.png`. `stemma icon` creates it from the installer on any host:
an MSI supplies the icon it registers for Programs and Features (`ARPPRODUCTICON`)
and an EXE its first icon group, the icon Explorer shows. On a Mac the default
`glassy` presentation draws that artwork with the same renderer as macOS
applications, so both platforms' icons look alike; elsewhere `raw` writes the
largest frame unchanged. An installer without a registered icon reports
`no artwork`; commit a square PNG or share the macOS document's asset instead.

## Signature

Declare the selected setup entry point's signing expectation:

```yaml
signatures:
  - subject:
      path: bin/setup.exe
    signer: authenticode:4a6519d3c145fc3838df20b3009980fe59b9bc68ee5871e59aaa0097a523e333 # Google LLC
```

Paths are relative to the prepared setup directory. A scalar MSI or EXE uses
`path: .`. Only the selected entry point is a signing subject; auxiliary executables
are outside this scope.

`stemma signature WindowsSoftware/<name>` derives a complete fragment without
requiring an existing declaration. It reports unsigned installers normally, as
`unsigned: true` instead of `signer`. The two assertions are mutually exclusive.
Omitting `signatures` makes no assertion. Preparation fails when the observed state
or publisher differs; invalid and unsupported signatures also fail derivation.

The signer identifies the certificate subject together with the issuing
authority's public key. Routine certificate renewal keeps it while a new authority
or publisher requires review. Timestamp countersignatures fix the time at which
certificate validity is judged. Windows trust policy such as SmartScreen is not
assessed. See [signature limits](limitations.md#signatures).
