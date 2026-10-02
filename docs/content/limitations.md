# Runtime and limitations

Stemma's runner and the software's target platform are separate. The CLI builds
for macOS and Linux on amd64 and arm64, and for Windows on amd64. Built-in inspection and packaging
use portable Go implementations. Only the glassy icon presentation, an
option of `stemma icon`, needs macOS.

Plugins can impose additional runner or tool requirements. Stemma checks declared
requirements before expensive processing or destination mutation and reports the
operation's setup instructions. It cannot discover undeclared dependencies in
arbitrary plugin code.

## Files and packages

| Operation             | Supported scope                                                                                                          |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Mac PKG inspection    | Flat XAR packages and supported component payloads; streams file contents while retaining bounded inventory metadata     |
| DMG inspection        | Raw, ADC, zlib, bzip2, LZFSE and LZMA chunks; supported HFS+, HFSX and single-volume APFS filesystems                    |
| Application DMGs      | One application at the root of an LZFSE, zlib or LZMA compressed HFS+ image; bytes, modes and confined relative symlinks |
| Custom Mac packaging  | Unsigned component packages with gzip or xz payloads, payload layouts, endpoint hooks and temporary script resources     |
| MSI inspection        | Reads MSI database metadata without Windows or executing the installer                                                   |
| EXE preparation       | Preserves the vendor installer; commands, version-specific detection and other installation semantics are declared       |
| Intune Win32 wrapping | Portable `.intunewin` preparation with a 2 GiB implementation bound                                                      |
| Icon creation         | `stemma icon` extracts icon artwork on any host; the glassy presentation needs the macOS renderer                        |

PKG inspection bounds entry counts and retained path metadata, rather than imposing
a small total payload size. Exceptionally large inventories can still hit those
bounds. Unsupported DMG layouts and codecs fail; not every image accepted by
macOS is supported by the portable reader.

DMG inspection reads the filesystem through compressed chunks, without creating a
raw filesystem image. Applications are verified inside the image, and
icon rendering copies out only the files the renderer reads. Published vendor
installers retain their original bytes. Leave enough working disk space for
extracted content and destination preparation.

The pipeline consumes catalog-selected vendor artifacts. Inspection does not run
installer contents, and extraction remains confined to its destination. File,
entry and output limits catch malformed or unexpectedly large inputs; they do not
provide a sandbox or guarantee bounded resource use for deliberately adversarial
files.

New application DMGs retain bytes, modes, confined relative symlinks and archive
extended attributes, including resource forks. Archive metadata must identify an
exact existing entry; conflicting attributes and symlink parents are rejected.
New PKG payloads omit extended attributes, ACLs and resource forks and reject code
signatures stored in those attributes. Local tree imports also reject these
signatures. Existing vendor PKGs and DMGs retain their original bytes.

## Signatures

Each signed subject is verified independently against its expected publisher: Developer ID signatures over PKGs and application
bundles, and Authenticode signatures over MSI and EXE files. Apple verification
covers the shapes `codesign` writes today: `files2` envelopes, versioned and
shallow frameworks, nested bundles and executables, and nested generic code, such
as scripts and data files, whose signature `codesign` keeps in extended
attributes. Generic code verifies inside vendor DMGs and archives carrying AppleDouble or
PAX attributes. Local trees without those attributes cannot verify generic code. Of Apple's requirement
language, only the Developer ID requirement `codesign` records by default is
evaluated, and only for nested code that replaced the code its app sealed; other
requirements accept only the sealed code. Legacy envelopes and detached signature
files are rejected rather than emulated, identically on every host. Signature absence is a
normal observation for supported formats; partial and ad-hoc signatures are not
unsigned.
Windows verification anchors at the issuing authority recorded in the signer
value and never consults the operating system's root store. Neither asserts
notarisation, Gatekeeper, SmartScreen or WDAC policy, nor certificate revocation.

Installer scripts and application executables are never run to infer their effects.
An installer containing a downloader cannot prove what that downloader eventually
installs. Set endpoint detection from the vendor's installation behaviour.

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
the CLI, but must retain reviewed locks and credentials.

Retention is provider-owned and reference-aware. It is not a hard storage ceiling,
an app retirement policy or an automatic rollback mechanism. See
[publishing](publishing.md#identity-and-retention).

Icon creation extracts the artwork software carries on any host and styles it with
the macOS renderer only where one exists. Reconciliation publishes committed PNG
files and never renders, so every runner produces the same result.
