# Getting started

Install a binary for your runner's operating system and architecture from
[Releases](https://github.com/woodleighschool/stemma/releases), and put `stemma`
(`stemma.exe` on Windows) on your `PATH`. To use the current source checkout, see
[development](development.md).

A catalog repository can pin the release with [Mise](https://mise.jdx.dev) instead,
so local runs and CI install the same verified binary:

```sh
mise use github:woodleighschool/stemma
mise lock
```

Commit the Mise configuration and `mise.lock`.

The runner is the computer executing Stemma. It can prepare software for a different
operating system; see [runtime requirements](limitations.md).

## Create a catalog

This walkthrough prepares Chrome for a local Munki repository. It needs no service
credentials and does not install Chrome on the runner. For Windows and Intune,
use the same command sequence with the [Windows example](windows-software.md).

Create a directory and initialise Git:

```sh
mkdir my-catalog
cd my-catalog
git init
mkdir software
```

Save this as `stemma.yaml`:

```yaml
apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: my-catalog
spec:
  imports:
    - software/**/*.yaml
  destinations:
    munki:
      operation: munki
      config:
        path: munki-repo
```

Save this as `software/chrome.yaml`:

```yaml
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: chrome
spec:
  source:
    url: https://dl.google.com/dl/chrome/mac/universal/stable/gcem/GoogleChrome.pkg
  application:
    bundle_id: com.google.Chrome
  destinations:
    munki:
      pkginfo:
        catalogs:
          - testing
        description: Google Chrome.
```

Add `.stemma/` and `munki-repo/` to `.gitignore`. The first contains local state;
the second will contain this walkthrough's published repository.

## Prepare and review

Run from the catalog:

```sh
stemma validate
stemma prepare
git diff -- stemma.lock.yaml
stemma plan
```

`validate` checks the documents and operation contracts. `prepare` downloads the
installer, inspects it and writes the lockfile. On its first creation, the lockfile
is untracked: open it directly as well as checking `git status`.

`plan` reads the destination and reports the changes it would make. It can fetch
locked inputs into an empty cache, but does not write to the destination. It is the
dry run.

The selected bundle supplies application metadata. You do not need to repeat its
version and bundle identifier as separate extraction steps.

Commit `stemma.yaml`, `software/` and `stemma.lock.yaml` when the inputs are right.

## Publish

```sh
stemma apply
```

For this example, publication writes pkginfo, installer content and catalogs under
`munki-repo/`. It does not enrol or target a device. Publishing to an existing
repository can affect clients already using it; destination configuration defines
that behaviour.

Run `stemma plan` again to see whether anything remains to reconcile. `apply`
re-reads destination state; it does not replay a saved plan.

## Pick up a new version

```sh
stemma update MacSoftware/chrome
stemma prepare MacSoftware/chrome
git diff -- stemma.lock.yaml
stemma plan MacSoftware/chrome
```

`update` explicitly checks upstream again. `prepare` keeps an existing remote pin.
Review and commit the new lock before applying. See [sources and updates](sources.md)
for local changes, cold caches and URLs whose content changes in place.
