# Windows software

`WindowsSoftware` starts with an existing vendor installer. Intune prepares the
upload envelope from that installer or setup directory. You do not author a build
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
        tenant_id: ${INTUNE_TENANT_ID}
        client_id: ${INTUNE_CLIENT_ID}
        client_secret: ${INTUNE_CLIENT_SECRET}
  components:
    windows-win32:
      destinations:
        intune:
          type: win32
          allowedArchitectures: x64
          minimumSupportedWindowsRelease: Windows11_24H2
          installExperience:
            runAsAccount: system
            deviceRestartBehavior: basedOnReturnCode
          returnCodes:
            - returnCode: 0
              type: success
            - returnCode: 1707
              type: success
            - returnCode: 3010
              type: softReboot
            - returnCode: 1641
              type: hardReboot
            - returnCode: 1618
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
      description: Google Chrome.
```

```sh
stemma validate
stemma prepare WindowsSoftware/chrome
stemma plan WindowsSoftware/chrome
```

The selected MSI supplies descriptive metadata, standard silent `msiexec` install
and uninstall commands, and ProductCode/version detection. Explicit destination
values override these defaults. `apply` publishes when you are ready.

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
      displayName: Visual Studio Code
      description: Visual Studio Code system installation.
      publisher: Microsoft
      installCommandLine: "VSCodeSetup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /MERGETASKS=!runcode"
      uninstallCommandLine: '"C:\Program Files\Microsoft VS Code\unins000.exe" /VERYSILENT /SUPPRESSMSGBOXES /NORESTART'
      rules:
        - "@odata.type": "#microsoft.graph.win32LobAppFileSystemRule"
          ruleType: detection
          path: 'C:\Program Files\Microsoft VS Code'
          fileOrFolderName: Code.exe
          check32BitOn64System: false
          operationType: exists
          operator: notConfigured
```

This rule detects presence. It intentionally does not enforce a particular VS Code
version. To manage a minimum version, use `operationType: version`,
`operator: greaterThanOrEqual` and a `comparisonValue` matching the installed file's
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
    installCommandLine: 'msiexec /i "Vendor.msi" TRANSFORMS="Organisation.mst" /qn /norestart'
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
rules:
  - "@odata.type": "#microsoft.graph.win32LobAppRegistryRule"
    ruleType: detection
    keyPath: 'HKEY_LOCAL_MACHINE\SOFTWARE\Example\Client'
    valueName: Version
    check32BitOn64System: false
    operationType: version
    operator: greaterThanOrEqual
    comparisonValue: "2.0.0"
```

Use a real vendor key and version. `greaterThanOrEqual` accepts an already-newer
installation; equality alone does not. Native file and registry rules can be
combined. PowerShell detection must be the only rule, with base64-encoded UTF-8
source in `scriptContent`, and explicit `runAs32Bit` and `enforceSignatureCheck`
where needed.

Intune considers a detection script successful only when it exits zero and writes
to stdout; stderr can make detection fail. Stemma does not execute detection scripts
to prove their endpoint behaviour. See Microsoft's
[detection documentation](https://learn.microsoft.com/en-us/intune/app-management/deployment/add-win32).

For example, save this as `detect.ps1` and test it on the intended Windows host:

```powershell
$app = 'C:\Program Files\Microsoft VS Code\Code.exe'
if (Test-Path -LiteralPath $app -PathType Leaf) {
    Write-Output 'Detected'
    exit 0
}
exit 1
```

Encode the file with PowerShell:

```powershell
[Convert]::ToBase64String([IO.File]::ReadAllBytes((Resolve-Path './detect.ps1')))
```

Paste that output as `scriptContent` in a rule of this shape:

```yaml
rules:
  - "@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule"
    ruleType: detection
    enforceSignatureCheck: false
    runAs32Bit: false
    scriptContent: REPLACE_WITH_BASE64_OUTPUT
```

For simple presence detection, the native file rule above avoids a script. Use
script detection when the vendor's installed state needs more than a native rule.

Dependencies and supersedence are [publication relationships](publishing.md#intune-relationships),
not setup-directory inputs.
