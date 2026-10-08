# BuildMacPkg

Use `BuildMacPkg` for files you need to install at particular locations: fonts,
branding assets, configuration files or a package containing installer scripts.
An ordinary vendor application usually belongs in [MacSoftware](mac-software.md),
which can also [publish it in a package](mac-software.md#publish-an-application-in-a-package).

## Package a directory

Place your fonts in `Fonts/` beside this file. The first document builds the
package; the second publishes its `installer` output:

```yaml
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: fonts
spec:
  inputs:
    fonts:
      path: Fonts
  package:
    identifier: org.example.fonts
    version: "1.0"
  payload:
    /Library/Fonts:
      $input: fonts
      mode: "0755"
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: fonts
spec:
  source:
    resource:
      kind: BuildMacPkg
      name: fonts
      output: installer
  destinations:
    munki:
      pkginfo:
        description: Organisation fonts.
```

```sh
stemma update MacSoftware/fonts
stemma prepare MacSoftware/fonts
stemma plan MacSoftware/fonts
```

The build runs first. It can also run alone with `stemma prepare BuildMacPkg/fonts`.
Building does not require a destination.

## Describe the payload

Payload keys are installation paths relative to the package's `/` installation
root. Each entry can select an input file or tree, select a path within an input,
contain literal text, declare a relative symlink, or create a directory:

```yaml
payload:
  /Library/Desktop Pictures/Branding:
    $input: branding
    path: Wallpapers
    mode: "0755"
    uid: 0
    gid: 0
  /Library/Application Support/Example/version.txt:
    content: |
      1.0
    mode: "0644"
```

Declare `branding` under `inputs` using a [shared resolver](sources.md). Multiple
named inputs can come from repository assets or network downloads.

`mode` applies to the mapped root. Existing modes within a tree are preserved.
Ownership defaults to root:wheel; numeric `uid` and `gid` apply to the mapped
subtree without changing ownership on the runner. Quote octal modes in YAML.

Use `symlink: ../libexec/example/run` at `/usr/local/bin/example` to expose a
command from a private payload directory. Links stay relative and confined to the
package root; loops and symlink destination parents are rejected. `$input`,
`content` and `symlink` are mutually exclusive. An entry with none creates a
directory. Existing confined relative symlinks in selected inputs are preserved. Extended attributes, ACLs and resource forks are not
carried into new payload trees. Vendor installers retain their original bytes.

## Select contents from an input

An omitted `path` uses the original input. For a file, this preserves its bytes;
for a directory, it copies the directory tree. An explicit `path` selects an exact
member of a directory, ZIP, TAR or DMG. `path: .` selects the contents root:

```yaml
inputs:
  vendor:
    path: Assets/Vendor.zip
payload:
  /Library/Application Support/Example:
    $input: vendor
    path: resources
```

The same declaration works when `vendor` is a directory, TAR or DMG containing
`resources`. Build paths are exact, without globs or automatic app selection.
An ordinary file permits only itself; PKGs retain their package semantics and
cannot be traversed as a generic directory.

A resolver can select a useful content root. Homebrew formula inputs start at
the formula's files: omit `path` to copy them all, or use `path: libexec` to select
that directory. The package still declares where those files belong and which
commands to expose. `{{ inputs.tool.version }}` uses the resolved input version;
Homebrew includes both formula revision and bottle rebuild in this value.

The lock always identifies the original input. Selecting another member changes
the build, not the source lock. ZIP/TAR contents expand once per input in the
build workspace. DMG members are read through the disk-image reader without
mounting the image or expanding unrelated files. Temporary build files disappear
when preparation finishes.

## Include installer scripts and resources

`scripts` maps names in the package's temporary Scripts area to literal text or
input selections. Root files named `preinstall` and `postinstall` become executable
installer hooks automatically. Other names hold files or trees those hooks use. A Scripts
area must contain at least one regular hook; hooks and resources are never run by
Stemma.

An Adobe-style wrapper can carry the original DMG beside its installer hook:

```yaml
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata:
  name: vendor-wrapper
spec:
  inputs:
    media:
      path: Assets/Vendor.dmg
    postinstall:
      path: Scripts/postinstall
  package:
    identifier: org.example.vendor-wrapper
    version: "1.0"
  scripts:
    postinstall:
      $input: postinstall
    installer.dmg:
      $input: media
```

On the endpoint, the hook can find `installer.dmg` beside `$0`, mount it, run the
vendor installer and detach it on exit. macOS Installer owns the temporary Scripts
area. The package does not install that DMG into a persistent payload directory.

A Sophos-style ZIP needs no intermediate DMG. Copy its contents into the same
temporary area, including any configuration files next to the installer app:

```yaml
inputs:
  vendor:
    path: Assets/Vendor.zip
scripts:
  postinstall: |-
    #!/bin/zsh --no-rcs
    set -e
    directory="${0:A:h}/installer"
    "$directory/Vendor Installer.app/Contents/MacOS/Installer" --quiet
  installer:
    $input: vendor
    path: .
```

To select a hook within a declared tree, use `{$input: hooks, path: postinstall}`.

The hook runs the app from `installer/` beside `$0`. Use `payload` instead when
files must remain installed after the hook returns. A vendor-supplied PKG normally
belongs in `MacSoftware`, preserving its original bytes and installer behaviour.

Every built package includes a payload, so macOS records its identifier and
version in a receipt after successful installation. Omit `payload` when the
package only runs scripts; its payload is then empty. The receipt records that
the package ran successfully, not whether the script's effects still hold.

Input selections preserve file modes and confined relative symlinks. Selected paths
cannot traverse a symlink; links inside copied trees must remain within that
selection. Overlapping entries fail. Each output area is limited to 100,000 entries
and 16 GiB; individual files must fit the package writer's 32-bit size field.
Hooks are limited to 1 MiB each, independently of their accompanying media.
Archive expansion is also bounded to 100,000 entries and 16 GiB. Output dates are
fixed at the Unix epoch, and entries are written in sorted order.

Keep destination behaviour out of the builder. For example, a GarageBand content
package can carry a downloader and its installer hook, while Munki's detection and
configuration scripts belong in the publishing document's `pkginfo`.

The builder produces unsigned component PKGs. It does not sign packages, build
distribution installers or execute an AutoPkg-style processor chain.
`package.version` is declared directly or through an expression using environment
values or input metadata. An input's `facts` inventory the applications and
packages it holds, keyed by path, as `MacSoftware` inspection does:

```yaml
package:
  identifier: com.example.pkg.vendor
  version: "{{ inputs.vendor.facts['Vendor Installer.app'].app.version }}-1"
```

An input is inspected when an expression reads its facts or signing expectations
cover its selected content. Vendor-specific
discovery and other metadata extraction belong in a resolver or resource plugin.
Set `package.filename` only when the default output name needs to be overridden.

## Compress the payload

`package.compression` controls how the package payload is compressed:

```yaml
package:
  identifier: org.example.fonts
  version: "1.0"
  compression: xz
```

| `compression`    | Package size   | Expand time on a Mac | Compatibility        |
| ---------------- | -------------- | -------------------- | -------------------- |
| `gzip` (default) | baseline       | baseline             | any macOS            |
| `xz`             | 14–23% smaller | 12–29% faster        | macOS 10.10 or later |

`xz` is useful when package size matters. It takes 6–8x as long to create, but
macOS expands the payload in parallel, so installation is no slower than with
`gzip`, though it uses 3–4x the CPU.

The size range above comes from Chrome, Visual Studio Code and Minecraft
Education packaged as payloads.

`xz` uses PBZX, the same payload format produced by `pkgbuild --compression latest`,
with 16 MiB independently compressed blocks. Stemma needs 5–9x the CPU to read
an xz payload during operations such as `MacSoftware` inspection. The
Scripts archive remains gzip-compressed, matching `pkgbuild`.

## Verify the wrapped input

`signatures` asserts the signing state of subjects the resolved payload and scripts
layout consumes:

```yaml
signatures:
  - input: vendor
    signer: apple:developer-id:JQ525L2MZD # Adobe Inc.
```

An entry without `subject` covers the only signing subject the layout consumes
from the input, such as a PKG or the one app it copies. Where it consumes several,
each needs an entry with `subject.path`, its path within that input. Selecting one
app excludes its unused siblings; copying a whole directory, archive or DMG
includes its top-level apps and packages. A PKG is verified by its outer signature, never a component
receipt. When `signatures` is present, every consumed signing subject needs an
expectation, except a package another `BuildMacPkg` resource built. Literal text
and ordinary data files are outside this scope. Without `signatures` nothing is
verified, and an input that supplies an application or package produces a
recommendation to derive its expectation.

The [MacSoftware signing rules](mac-software.md#signature) also apply here,
including `unsigned: true`. Run `stemma signature BuildMacPkg/<name>` with no
placeholder declaration to derive every consumed subject's signing state.

Evidence records each input and subject separately. The built package stays
unsigned, so input verification cannot satisfy Intune line-of-business validation.
Its `build.signature` evidence records that state, and the `MacSoftware` resource
publishing it needs no `signatures`.
