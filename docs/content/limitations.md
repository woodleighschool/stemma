# Runtime and limitations

Stemma's runner and the software's target platform are separate. The CLI builds
for macOS, Linux and Windows on amd64 and arm64. Built-in inspection and packaging
use portable Go implementations. Only the glassy icon presentation, an authoring
option of `stemma icon`, needs macOS.

Plugins can impose additional runner or tool requirements. Stemma checks declared
requirements before expensive processing or destination mutation and reports the
operation's setup instructions. It cannot discover undeclared dependencies in
arbitrary plugin code.

## Files and packages

| Operation             | Supported scope                                                                                                       |
| --------------------- | --------------------------------------------------------------------------------------------------------------------- |
| Mac PKG inspection    | Flat XAR packages and supported component payloads; streams file contents while retaining bounded inventory metadata  |
| DMG inspection        | Raw, ADC, zlib, bzip2, LZFSE and LZMA chunks; supported HFS+, HFSX and single-volume APFS filesystems                 |
| Application DMGs      | One application at the root of a zlib-compressed HFS+ image; bytes, modes and confined relative symlinks              |
| Custom Mac packaging  | Unsigned component packages, payload layouts and endpoint installer scripts                                           |
| MSI inspection        | Reads MSI database metadata without Windows or executing the installer                                                |
| EXE preparation       | Preserves the vendor installer; commands, version-specific detection and other installation semantics remain authored |
| Intune Win32 wrapping | Portable `.intunewin` preparation with a 2 GiB implementation bound                                                   |
| Icon authoring        | `stemma icon` extracts icon artwork on any host; the glassy presentation needs the macOS renderer                     |

PKG inspection bounds entry counts and retained path metadata, rather than imposing
a small total payload size. Exceptionally large inventories can still hit those
bounds. Unsupported DMG layouts and codecs fail; not every image accepted by
macOS is supported by the portable reader.

DMG inspection reads the filesystem through compressed chunks, without creating a
raw filesystem image. A selected application is verified inside the image, and
icon rendering copies out only the files the renderer reads. Published vendor
installers retain their original bytes. Leave enough working disk space for
extracted content and destination preparation.

The pipeline consumes catalog-selected vendor artifacts. Inspection does not run
installer contents, and extraction remains confined to its destination. File,
entry and output limits catch malformed or unexpectedly large inputs; they do not
provide a sandbox or guarantee bounded resource use for deliberately adversarial
files.

New payload trees and application DMGs retain bytes, modes and confined relative
symlinks, which is what a Git checkout carries. Extended attributes, ACLs and resource forks are left
behind, and AppleDouble sidecars in vendor archives are skipped.
This is distinct from preserving an existing vendor PKG or DMG unchanged.

## Signatures

Signature verification establishes that acquired bytes are the content one
expected publisher signed: Developer ID signatures over PKGs and application
bundles, and Authenticode signatures over MSI and EXE files. Apple verification
covers the shapes `codesign` writes today: `files2` envelopes, versioned and
shallow frameworks, nested bundles and executables. Legacy envelopes, detached
signature files and nested code replaced under Apple's requirement language are
rejected rather than emulated, identically on every host.
Windows verification anchors at the issuing authority recorded in the signer
value and never consults the operating system's root store. Neither asserts
notarisation, Gatekeeper, SmartScreen or WDAC policy, nor certificate revocation.

Installer scripts and application executables are never run to infer their effects.
An installer containing a downloader cannot prove what that downloader eventually
installs. Author endpoint detection from the vendor's installation behaviour.

Stemma's Intune wrapper does not invoke Microsoft's content-preparation tool.
Using that tool separately requires its supported Windows runtime and
[.NET Framework prerequisite](https://github.com/microsoft/Microsoft-Win32-Content-Prep-Tool).
Successful format verification or API upload is not evidence of installation on a
managed endpoint.

## Publication

Native destination schemas describe the supported fields, not every field exposed
by the service. Windows package construction, Remediations, general script-policy
resources and arbitrary workflows are not built-in contracts.

There is no distributed transaction across destinations, background scheduler,
Git watcher or continuous reconciliation controller. An external runner can invoke
the CLI, but must retain reviewed locks, credentials and durable destination state.

Retention is provider-owned and reference-aware. It is not a hard storage ceiling,
an app retirement policy or an automatic rollback mechanism. See
[publishing](publishing.md#identity-and-retention).

Icon authoring extracts the artwork software carries on any host and styles it with
the macOS renderer only where one exists. Reconciliation publishes committed PNG
files and never renders, so every runner produces the same result.
