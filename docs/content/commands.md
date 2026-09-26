# Commands

Run commands from a catalog. Stemma discovers its Git root and `stemma.yaml`.
`--root` selects another project directory; `--config` selects a Project file.

## Catalog workflow

| Command                           | Purpose                                                                   |
| --------------------------------- | ------------------------------------------------------------------------- |
| `stemma validate`                 | Check the catalog as written, without environment values or acquisition   |
| `stemma update [Kind/name...]`    | Discover current inputs and update their locks                            |
| `stemma prepare [Kind/name...]`   | Lock and prepare inputs without publication                               |
| `stemma signature [Kind/name...]` | Derive the verified signer of each published artifact                     |
| `stemma artifact Kind/name`       | Prepare one resource from the lockfile and print the path of its artifact |
| `stemma icon [Kind/name...]`      | Create missing declared icons from the software's own artwork             |
| `stemma plan [Kind/name...]`      | Read destinations and report proposed changes                             |
| `stemma apply [Kind/name...]`     | Re-read and reconcile destinations once                                   |

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

`artifact` prepares one resource and prints only the absolute path of its
`installer` output, or of the output `--output` names. It uses the reviewed
lockfile: an input without a reviewed entry fails until `stemma update` records
it. `--no-input-lock` instead resolves the inputs of the resource and the builds
it consumes from their sources as they are now, to show what an update would
prepare. Either way the lockfile stays unchanged and plugins must match it. No
destination receives the artifact. The path is a copy in the cache that each
run replaces and `stemma cache prune` removes. Progress and errors stay on
stderr, and the live tree needs only stderr to be a terminal, so the command
composes with other tools:

```sh
stemma inspect "$(stemma artifact MacSoftware/foo)"
stemma inspect "$(stemma artifact MacSoftware/foo --no-input-lock)"
pkgutil --check-signature "$(stemma artifact MacSoftware/foo)"
```

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

`validate --resolved`, `schema --output-file -` and `operations` print JSON documents.
`schema` requires an explicit output file and includes the locally loaded plugins.
`--builtins` generates the default schema without loading a catalog; it uses the
same registry and schema composition as project generation.
`inspect` describes a local file or directory from its own metadata, without
executing it or loading a project; `--json` prints its complete facts. Pass it
the path `artifact` prints to inspect what Stemma prepares for a resource.
`validate` needs no credentials. `validate --resolved` also evaluates every
environment value, as runs do, and prints the merged configuration with connection
settings resolved. It needs every referenced variable and may expose secrets: do
not share it without reviewing it.

## Plugins and cache

```sh
stemma plugins list
stemma plugins install
stemma plugins update
stemma plugins publish IMAGE --goreleaser dist
stemma cache path
stemma cache prune
```

`plugins publish` pushes a GoReleaser release of plugin bundles as an OCI platform
index; see [writing plugins](writing-plugins.md#distribute-a-bundle).

`--cache-dir` / `STEMMA_CACHE_DIR` relocates disposable cached content.
`reconcile --state-dir` / `STEMMA_STATE_DIR` relocates the applied marker.
`cache prune` does not remove published packages.

## Reports and diagnostics

Outcome commands write each resource's report to stdout as it finishes, then the
run's totals. Plan and apply show changed, failed and blocked resources with
before and after values; update shows the input changes it locks, prepare shows
newly prepared resources, icon shows changed artwork and signature shows every
derived signer. Multi-line values, such as scripts, show their line counts
instead of their text. `--all` includes unchanged resources; totals always
describe the whole run. `--json` writes one JSON document with the same
selection when the run ends. Reconcile prints the reviewed commit's publication,
then looks up every resource before pushing each proposal, printing it as it is
pushed; in a terminal, each looked-up resource also leaves its outcome line.

```sh
stemma plan
stemma plan --json --all > plan.json
stemma prepare --all
```

When stdout and stderr are both terminals, a live tree on stderr shows each
unfinished resource's operations with what they work on, the steps of the
operation in progress and a bar for transfers of known size. A finished resource
replaces its tree with its report, or with its outcome line when the report
leaves it out, and reports are coloured. Redirected output and CI get the same
reports as plain text, without the tree or outcome lines. `NO_COLOR` disables
colour. Warnings go to stderr as they happen; in JSON mode they are part of the
document. A failed command exits nonzero and shows each failure once: in its
report, or after `Error:` on stderr when the failure stopped the run before the
report could show it.

Ctrl-C requests cancellation and workspace cleanup; results already shown remain.
Press Ctrl-C again to exit immediately if cleanup or a native operation is
taking too long.

Acquisition, preparation and destination failures are local: independent resources
continue, and consumers of unavailable resource outputs are reported as `blocked`
with `blocked_by` resource keys in JSON. The command emits the selected results
before exiting nonzero. Global configuration or structural failures and context
cancellation stop the run immediately and can leave a partial report.

Do not infer success solely from an artifact appearing in a report. Successful
resources can update the lockfile even when the command exits nonzero; failed or
blocked resources keep their previous reviewed entries. An absent `lock_changed`
means the lockfile comparison did not complete.

## Standalone packaging

```sh
stemma package pkg --help
stemma package intunewin --help
```

These expose low-level packaging for direct use. Catalogs normally use
`BuildMacPkg` or the Intune destination instead. `stemma <command> --help` is the
reference for the flags supported by your installed binary.
