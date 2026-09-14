# Commands

Run commands from a catalog. Stemma discovers its Git root and `stemma.yaml`.
`--root` selects another project directory; `--config` selects a Project file.

## Catalog workflow

| Command                         | Purpose                                                                      |
| ------------------------------- | ---------------------------------------------------------------------------- |
| `stemma validate`               | Check composition, schemas and operation contracts before acquiring software |
| `stemma update [Kind/name...]`  | Discover current inputs and update their locks                               |
| `stemma prepare [Kind/name...]` | Lock and prepare inputs without publication                                  |
| `stemma plan [Kind/name...]`    | Read destinations and report proposed changes                                |
| `stemma apply [Kind/name...]`   | Re-read and reconcile destinations once                                      |

For example, `stemma plan MacSoftware/chrome` selects one document. Required build
references are prepared first. Use `apiVersion/Kind/name` when needed to resolve an
ambiguous identity. Omitting selectors processes the catalog.

`apply --refresh-icons` refreshes application icons across the catalog; resource
selectors compose with it. Run it on macOS to use the current native system artwork.
`plan --refresh-icons` previews those changes without publishing. These flags
rerender icons and permit their replacement without forcing installer or unrelated
metadata writes. Normal runs retain existing icons and create missing ones.

`--offline` requires cached network inputs and plugin bundles; destination calls
are still allowed. Only `plan` is the publication dry run. See
[sources](sources.md) for lock behaviour.

## Inspection and configuration

```sh
stemma inspect installer.pkg
stemma validate --resolved
stemma schema
stemma schema --project --offline
stemma operations
stemma version
```

`inspect` reads artifact metadata without executing the installer.
`validate --resolved` prints merged configuration and may expose expanded
environment values: do not share it without reviewing it.

## Plugins and cache

```sh
stemma plugins list
stemma plugins install
stemma plugins update
stemma cache path
stemma cache prune
```

`--cache-dir` / `STEMMA_CACHE_DIR` relocates disposable cached content.
`--state-dir` / `STEMMA_STATE_DIR` relocates durable destination bindings.
`cache prune` does not remove destination bindings or published packages.

## Reports and diagnostics

Stdout contains command reports; stderr contains progress and diagnostics.

```sh
stemma plan --output json
stemma prepare --log-format json --output json
stemma prepare --verbose --no-progress
```

Use `--quiet` (`-q`) for warnings and errors, `--verbose` (`-v`) or `--debug` (`-d`)
for debug diagnostics, or `--log-level debug|info|warn|error`. `--no-progress`
disables terminal animation. `NO_COLOR` disables colours. These choices do not
suppress stdout reports.

Ctrl-C requests cancellation and workspace cleanup. Press Ctrl-C again to exit
immediately if cleanup or a native operation is taking too long.

A failed run can return partial results with an `error`. Do not infer success
solely from an artifact appearing in a report. An absent `lock_changed` means the
lockfile comparison did not complete.

## Standalone packaging

```sh
stemma package pkg --help
stemma package intunewin --help
```

These expose low-level packaging for direct use. Catalogs normally use
`BuildMacPkg` or the Intune destination instead. `stemma <command> --help` is the
reference for the flags supported by your installed binary.
