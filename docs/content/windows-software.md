# Windows software

`WindowsSoftware` starts with an existing vendor installer. Intune prepares the
upload envelope from that installer or setup directory. You do not declare a build
resource just to produce `.intunewin`.

## Configure Intune

Start a Project with these connection settings and shared Win32 defaults:

```yaml
apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: my-catalog
spec:
  imports:
    - software/**/*.yaml
  destinations:
    intune:
      operation: intune
      config:
        tenant_id: "{{ env.INTUNE_TENANT_ID }}"
        client_id: "{{ env.INTUNE_CLIENT_ID }}"
        client_secret: "{{ env.INTUNE_CLIENT_SECRET }}"
  components:
    windows-win32:
      destinations:
        intune:
          architecture: x64
          minimum_windows_release: Windows11_24H2
          install_experience:
            run_as: system
            restart: based_on_return_code
          return_codes:
            - code: 0
              type: success
            - code: 1707
              type: success
            - code: 3010
              type: soft_reboot
            - code: 1641
              type: hard_reboot
            - code: 1618
              type: retry
```

Supply credentials through your shell or runner's secret management. The Entra app
needs Graph application permissions appropriate to app management; see
[publishing](publishing.md#intune). Change architecture, minimum release and
installation context to match the vendor and your devices.

## Publish an enterprise MSI

Save as `software/chrome.yaml`:

```yaml
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
      display_name: Google Chrome
      description: Google Chrome.
      publisher: Google LLC
```

```sh
stemma validate
stemma prepare WindowsSoftware/chrome
stemma plan WindowsSoftware/chrome
```

The selected MSI supplies its MSI information, standard silent `msiexec` install
and uninstall commands, and ProductCode/version detection. Explicit destination
values override these defaults. The display name, description and publisher are
always set. `apply` publishes when you are ready.

## Publish an EXE

EXE switches are vendor-specific. This uses VS Code's **system** installer, its
[documented Inno Setup support](https://code.visualstudio.com/docs/setup/windows)
and the installed uninstaller's silent switches:

```yaml
apiVersion: stemma/v1alpha1
kind: WindowsSoftware
metadata:
  name: visual-studio-code
spec:
  extends: windows-win32
  source:
    url: https://update.code.visualstudio.com/latest/win32-x64/stable
    filename: VSCodeSetup.exe
  destinations:
    intune:
      display_name: Visual Studio Code
      description: Visual Studio Code system installation.
      publisher: Microsoft
      install_command: "VSCodeSetup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /MERGETASKS=!runcode"
      uninstall_command: '"C:\Program Files\Microsoft VS Code\unins000.exe" /VERYSILENT /SUPPRESSMSGBOXES /NORESTART'
      detection:
        - type: file
          path: 'C:\Program Files\Microsoft VS Code'
          name: Code.exe
          property: exists
```

This rule detects presence. It intentionally does not enforce a particular VS Code
version. To manage a minimum version, use `property: version`,
`operator: greater_than_or_equal` and a `value` matching the installed file's
version. EXE version and detection values are not automatically inferred.

## Include a transform or wrapper

Add accompanying files to the setup content, preserving their relative names:

```yaml
source:
  path: Assets/Vendor.msi
content:
  files:
    Organisation.mst:
      path: Assets/Organisation.mst
destinations:
  intune:
    install_command: 'msiexec /i "Vendor.msi" TRANSFORMS="Organisation.mst" /qn /norestart'
```

This is a spec excerpt using the shared Win32 defaults. Intune includes the whole
assembled setup directory. A directory source instead names its entry point:

```yaml
source:
  path: Assets/Setup
content:
  setup_file: VendorSetup.exe
```

Files may use any shared input resolver. A wrapper can be the setup entry point,
but commands and detection must describe the actual installed product; MSI
defaults apply when the selected entry point is an MSI. See Microsoft's
[setup-folder model](https://learn.microsoft.com/en-us/intune/app-management/deployment/create-win32-package).

## Choose installed-product evidence

An MSI's ProductCode identifies a product release, not the stable Stemma item or
Intune app. A major upgrade may change ProductCode while publication still updates
the same bound app ID. A ProductCode rule cannot detect a different ProductCode.

Use file, registry or script detection when you need evidence spanning those
upgrades. A registry version rule, for example:

```yaml
detection:
  - type: registry
    key: 'HKEY_LOCAL_MACHINE\SOFTWARE\Example\Client'
    value_name: Version
    property: version
    operator: greater_than_or_equal
    value: "2.0.0"
```

Use a real vendor key and version. `greater_than_or_equal` accepts an already-newer
installation; equality alone does not. File and registry rules can be combined, and
`check_32bit` selects the 32-bit view on 64-bit Windows. A script rule must be the
only rule, with `run_as_32bit` and `enforce_signature_check` where needed.

Intune considers a detection script successful only when it exits zero and writes
to stdout; stderr can make detection fail. Stemma does not execute detection scripts
to prove their endpoint behaviour. See Microsoft's
[detection documentation](https://learn.microsoft.com/en-us/intune/app-management/deployment/add-win32).

For example, test this script on the intended Windows host:

```yaml
detection:
  - type: script
    script: |
      $app = 'C:\Program Files\Microsoft VS Code\Code.exe'
      if (Test-Path -LiteralPath $app -PathType Leaf) {
          Write-Output 'Detected'
          exit 0
      }
      exit 1
```

For simple presence detection, the file rule above avoids a script. Use
script detection when the vendor's installed state needs more than a native rule.

Dependencies and supersedence are [publication relationships](publishing.md#intune-relationships),
not setup-directory inputs.

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

Require the setup file to carry a complete, valid Authenticode signature from an
expected publisher:

```yaml
signature:
  signer: authenticode:4a6519d3c145fc3838df20b3009980fe59b9bc68ee5871e59aaa0097a523e333 # Google LLC
```

`stemma signature WindowsSoftware/<name>` derives the value from the acquired
installer, verifying it first. The value identifies the publisher by its
certificate subject together with the issuing authority's public key, so routine
certificate renewal keeps it while a new authority or publisher fails preparation
until the document is updated. Timestamp countersignatures fix the time at which
certificate validity is judged. Windows trust policy such as SmartScreen is not
assessed. See [signature limits](limitations.md#signatures).
