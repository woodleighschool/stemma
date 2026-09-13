# Building a Mac package

Use `BuildMacPkg` for files you need to install at particular locations: fonts,
branding assets, configuration files or a package containing installer scripts.
An ordinary vendor application usually belongs in [MacSoftware](mac-software.md).

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
stemma prepare MacSoftware/fonts
stemma plan MacSoftware/fonts
```

The build runs first. It can also run alone with `stemma prepare BuildMacPkg/fonts`.
Building does not require a destination.

## Describe the payload

Payload keys are installation paths relative to the package's `/` installation
root. Each entry can select an input file or tree, select a path within an input,
contain literal text, or create a directory:

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

An entry with neither `$input` nor `content` creates a directory. Confined relative
symlinks are supported; unsupported metadata is rejected rather than dropped.

## Include installer scripts

Add a script input and reference it by installer role:

```yaml
inputs:
  scripts:
    path: Scripts
scripts:
  preinstall:
    $input: scripts
    path: preinstall
  postinstall:
    $input: scripts
    path: postinstall
```

Those files are endpoint payload. Stemma packages them but never runs them. A
scripts-only package can omit `payload`.

Keep destination behaviour out of the builder. For example, a GarageBand content
package can carry a downloader and its installer hook, while Munki's detection and
configuration scripts belong in the publishing document's `pkginfo`.

The builder produces unsigned component PKGs. It does not sign packages, build
arbitrary distribution installers or execute an AutoPkg-style processor chain.
Set `package.filename` only when the default output name needs to be overridden.
