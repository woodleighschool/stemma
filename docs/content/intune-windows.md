# Windows apps in Intune

Publish a [WindowsSoftware](windows-software.md) installer or setup directory as
an Intune Win32 app. The destination creates the `.intunewin` upload envelope
from the prepared content. Installation commands, detection and assignments belong
to this destination.

See [publishing](publishing.md#intune) for authentication, assignments and
publication identity.

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
          architectures: [x64]
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
[publishing](publishing.md#intune). Change architectures, minimum release and
installation context to match the vendor and your devices; an x64 app that should
also install on Arm devices lists `[x64, arm64]`.

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
stemma update WindowsSoftware/chrome
stemma prepare WindowsSoftware/chrome
stemma plan WindowsSoftware/chrome
```

The selected MSI supplies its MSI information, standard silent `msiexec` install
and uninstall commands, and ProductCode/version detection. Explicit destination
values override these defaults. The display name, description and publisher are
always set. `apply` publishes when you are ready.

Windows Installer properties extend the standard install command:

```yaml
destinations:
  intune:
    msi_properties:
      PORTAL: vpn.example.com
      CONNECTMETHOD: on-demand
```

Values are quoted in name order, with a quote inside a value doubled. Set
`msi_properties` or a complete `install_command`, not both.

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
version. To manage a minimum version, use `property: version` with a `value`
matching the installed file's version, or omit `value` when the source reports the
installer's version. Stemma does not infer EXE commands or detection.

## Accompanying files

[WindowsSoftware](windows-software.md#include-accompanying-files) assembles the
setup directory, including transforms and wrapper scripts. Intune uploads the
whole directory and uses its selected setup file as the entry point. For an MSI
with a transform named `Organisation.mst`, add:

```yaml
destinations:
  intune:
    msi_properties:
      TRANSFORMS: Organisation.mst
```

MSI defaults apply when the setup file is an MSI. A wrapper script needs explicit
commands and detection for the product it installs.

## Choose installed-product evidence

An MSI's ProductCode identifies a product release, not the stable Stemma item or
Intune app. A major upgrade may change ProductCode while publication still updates
the same bound app ID. A ProductCode rule cannot detect a different ProductCode.

Use file, registry or script detection when you need evidence spanning those
upgrades. A file or registry version rule without `operator` and `value` accepts
the managed version or newer:

```yaml
detection:
  - type: registry
    key: 'HKEY_LOCAL_MACHINE\SOFTWARE\Example\Client'
    value_name: Version
    property: version
```

Use a real vendor key. The managed version is the setup MSI's ProductVersion, or
the version the source reports for its own installer. When the installed product
uses another version scheme, `version_file` takes the version the MSI's File table
records for one versioned file it installs:

```yaml
version_file: Zoom.exe
destinations:
  intune:
    detection:
      - type: file
        path: 'C:\Program Files\Zoom\bin'
        name: Zoom.exe
        property: version
```

MSI information and the default ProductCode rule keep the ProductVersion. An
explicit `operator` or `value` replaces its default; `greater_than_or_equal`
accepts an already-newer installation, while equality alone does not. File and
registry rules can be combined, and
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
