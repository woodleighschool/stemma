# Runtime and limitations

Stemma's runner and the software's target platform are separate. The CLI builds
for macOS, Linux and Windows on amd64 and arm64. Built-in inspection and packaging
use portable Go implementations, except native icon rendering.

Plugins can impose additional runner or tool requirements. Stemma checks declared
requirements before expensive processing or destination mutation and reports the
operation's setup instructions. It cannot discover undeclared dependencies in
arbitrary plugin code.

## Files and packages

| Operation             | Supported scope                                                                                                       |
| --------------------- | --------------------------------------------------------------------------------------------------------------------- |
| Mac PKG inspection    | Flat XAR packages and supported component payloads; streams file contents while retaining bounded inventory metadata  |
| DMG inspection        | Raw, ADC, zlib, bzip2 and LZMA chunks; supported HFS+, HFSX and single-volume APFS filesystems                        |
| Custom Mac packaging  | Unsigned component packages, payload layouts and endpoint installer scripts                                           |
| MSI inspection        | Reads MSI database metadata without Windows or executing the installer                                                |
| EXE preparation       | Preserves the vendor installer; commands, version-specific detection and other installation semantics remain authored |
| Intune Win32 wrapping | Portable `.intunewin` preparation with a 2 GiB implementation bound                                                   |
| Automatic icons       | Native system rendering on macOS; supported embedded PNG and PNG-backed ICNS elsewhere                                |

PKG inspection bounds entry counts and retained path metadata, rather than imposing
a small total payload size. Exceptionally large inventories can still hit those
bounds. Unsupported DMG layouts and codecs fail; not every image accepted by
macOS is supported by the portable reader.

DMG inspection creates a temporary sparse filesystem image. Published vendor
installers retain their original bytes. Leave enough working disk space for
inspection, extracted content and destination preparation.

New payload trees retain bytes, modes and confined relative symlinks. Resource
forks, ACLs, hardlinks and other unsupported payload metadata fail rather than
being silently discarded. On macOS, download provenance and tracking attributes
are omitted, and transparent filesystem compression is imported as ordinary bytes.
This is distinct from preserving an existing vendor PKG or DMG unchanged.

## Verification

Supported PKG and Mach-O verification checks artifact integrity, signatures and
optional exact certificate pins. It does not provide Apple certificate-chain
trust, revocation checking, notarisation assessment or Gatekeeper policy. Requesting
an unsupported verification mode fails.

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

Native application icon rendering is a macOS enhancement. Other supported runners
use portable application icon resources. Preparation remains usable without an icon;
vendor packages are not rendered as substitutes for their installed applications.
