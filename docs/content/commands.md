# Commands

Run commands from a catalog. Stemma discovers its Git root and `stemma.yaml`.
`--root` selects another project directory; `--config` selects a Project file.

## Catalog workflow

| Command                           | Purpose                                                                      |
| --------------------------------- | ---------------------------------------------------------------------------- |
| `stemma validate`                 | Check composition, schemas and operation contracts before acquiring software |
| `stemma update [Kind/name...]`    | Discover current inputs and update their locks                               |
| `stemma prepare [Kind/name...]`   | Lock and prepare inputs without publication                                  |
| `stemma signature [Kind/name...]` | Derive the verified signer of each published artifact                        |
| `stemma icon [Kind/name...]`      | Create missing declared icons from the software's own artwork                |
| `stemma plan [Kind/name...]`      | Read destinations and report proposed changes                                |
| `stemma apply [Kind/name...]`     | Re-read and reconcile destinations once                                      |

For example, `stemma plan MacSoftware/chrome` selects one document. Required build
references are prepared first. Use `apiVersion/Kind/name` when needed to resolve an
ambiguous identity. Omitting selectors processes every resource that is not
[suspended](catalogs.md#suspend-a-resource); a selector runs a suspended resource
and the builds it references.

A selector scopes evaluation as well as execution. Only the selected resources
and the resources they consume are checked against their operation contracts, so
an unrelated document that fails its own validation does not block the run. Use
`stemma validate` to check the whole catalog, including suspended resources.

`icon` writes [declared icons](mac-software.md#icons) to `icons/<name>.png` from
locked software. `--presentation raw` extracts the original artwork;
`--presentation glassy` uses the macOS renderer at `--size` pixels (512 by default).
The default, `auto`, chooses glassy on macOS and raw elsewhere.

Existing icons stay unchanged unless `--force` is set. Only resources needing an
icon are prepared. The command leaves the lockfile unchanged and does not contact
destinations. Commit the icons so publication sends the same bytes on every host.

`signature` acquires and prepares inputs like `prepare`, verifies each published
artifact against the signer it observes and prints the `signature` fragment to
add. It never writes documents, and a document that already names a different
signer fails. See [macOS](mac-software.md#signature) and
[Windows](windows-software.md#signature) signature policy.

`--offline` requires cached network inputs and plugin bundles; destination calls
are still allowed. Only `plan` is the publication dry run. See
[sources](sources.md) for lock behaviour.

## Automation

```sh
stemma reconcile
```

`reconcile` publishes the reviewed branch of the checkout you run it in and
proposes lock updates as pull requests in one finite run. The Project's
`spec.reconcile.source_control` names the provider; see
[automating updates](reconcile.md).

## Inspection and configuration

```sh
stemma inspect installer.pkg
stemma validate --resolved
stemma schema --output-file stemma.schema.json
stemma schema --offline --output-file -
stemma operations
stemma version
```

`inspect`, `validate --resolved`, `schema --output-file -` and `operations` print JSON documents.
`schema` requires an explicit output file and includes the locally loaded plugins.
`--builtins` generates the default schema without loading a catalog; it uses the
same registry and schema composition as project generation.
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
`reconcile --state-dir` / `STEMMA_STATE_DIR` relocates the applied marker.
`cache prune` does not remove published packages.

## Reports and diagnostics

Stdout contains command reports; stderr contains progress and diagnostics.
Commands that report an outcome print text, and `--json` prints the same report
as JSON.

```sh
stemma plan --json
stemma prepare --log-format json --json
stemma prepare --verbose --no-progress
```

Terminals show live progress. Plain logs name each stage once as it starts;
debug output and `--log-format json` also record each stage result and its
duration. A failed command ends with `Error:` lines on stderr, and each failed
resource shows its error once, in its own report. JSON logs record the raw error.

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
