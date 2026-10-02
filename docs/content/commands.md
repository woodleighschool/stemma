# Commands

Run commands from a catalog. Stemma discovers its Git root and `stemma.yaml`.
`--root` selects another project directory; `--config` selects a Project file.

## HTTPS trust

HTTPS uses the runner's system trust store, including Keychain trust on macOS
and the certificate store on Windows. Install private CAs there for ordinary use.
Trust belongs to the runner environment, outside catalog and destination settings.

To supply a PEM trust bundle for a run, set `SSL_CERT_FILE` before starting Stemma:

```sh
SSL_CERT_FILE=/absolute/path/ca-bundle.pem stemma plan
```

`SSL_CERT_DIR` selects certificate directories. These are Go's standard trust
overrides, inherited by plugin processes. With Go 1.27+, setting either also
replaces native certificate verification on macOS and Windows. Supply all roots
the run needs, including public CAs when it connects to public services; a private
CA file does not extend the native trust store. See
[Go's certificate loading rules](https://pkg.go.dev/crypto/x509#SystemCertPool).

## Catalog workflow

| Command                           | Purpose                                                                   |
| --------------------------------- | ------------------------------------------------------------------------- |
| `stemma validate`                 | Check the catalog as written, without environment values or acquisition   |
| `stemma update [Kind/name...]`    | Discover current inputs and update their locks                            |
| `stemma prepare [Kind/name...]`   | Prepare resources from the lockfile without publication                   |
| `stemma signature [Kind/name...]` | Derive signed or unsigned expectations for each signing subject           |
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
`update`, `artifact` and `icon` never reach a destination, so they skip the
destination checks the other commands make.

`update` writes the input locks and locks plugin tags and local paths. Every
other command uses the reviewed lockfile as it is: an input without a current
entry fails that resource until `stemma update` records it, and a plugin whose
entry is missing or stale does not load.

`prepare --changed-since REV` prepares only the resources whose preparation
differs from the catalog at the commit where `REV` and `HEAD` meet: their source
and preparation settings after components merge, their lock entries, resource
and input resolver operation identities, and Git changes under declared local
input paths. Destination metadata does not select
resources. Local paths containing environment expressions or outside the Git
root are selected only by declaration and lock changes. Plugin resolvers with
local inputs are selected for any changed project file because they do not
declare which files they read. New resources count as changed, and a changed build selects
the resources that consume it. Suspended resources stay out. The lockfile must
hold entries for exactly the declared inputs; entry and environment values are
read only for the resources the command prepares. Each catalog is interpreted by
the plugins its declarations and lock select, so comparison can execute both base
and current plugin code. Destination operation identities do not select resources
for preparation. A base catalog or required operation that cannot be loaded or
compared fails the comparison; verify such a change with an explicit preparation
run. The checkout needs its history back to that commit.

```sh
stemma prepare --changed-since origin/main
```

`icon` writes [declared icons](mac-software.md#icons) to `icons/<name>.png` from
locked software. `--presentation raw` extracts the original artwork;
`--presentation glassy` uses the macOS renderer at `--size` pixels (512 by default).
The default, `auto`, chooses glassy on macOS and raw elsewhere.

Existing icons stay unchanged unless `--force` is set. Only resources needing an
icon are prepared. The command leaves the lockfile unchanged and does not contact
destinations. Commit the icons so publication sends the same bytes on every host.

`signature` acquires and prepares inputs like `prepare`, records each subject's
signed or unsigned state and prints a complete `signatures` fragment. It never
writes documents and ignores existing signing expectations while deriving their
replacement. Invalid or unsupported signatures still fail. For `BuildMacPkg`, the
command reports the signing subjects its resolved layout consumes, labelled with
their input. See [macOS](mac-software.md#signature),
[Windows](windows-software.md#signature) and
[builder](building-packages.md#verify-the-wrapped-input) signing expectations.

`artifact` prepares one resource and prints only the absolute path of its
`installer` output, or of the output `--output` names. `--no-input-lock` instead
resolves the inputs of the resource and the builds it consumes from their sources
as they are now, to show what an update would prepare. No destination receives
the artifact. By default the path is a disposable copy in the cache; normal
maintenance protects it for 24 hours after use. `cache prune --all` removes it
immediately after active runs finish. Use `--output-file PATH` to export to a new
file or directory outside the cache. Its parent directory must exist; an existing
output is never overwritten. Exports belong to the caller and are never pruned. Progress and errors stay on
stderr, and the live tree needs only stderr to be a terminal, so the command
composes with other tools:

```sh
stemma inspect "$(stemma artifact MacSoftware/foo)"
stemma inspect "$(stemma artifact MacSoftware/foo --no-input-lock)"
pkgutil --check-signature "$(stemma artifact MacSoftware/foo)"
stemma artifact MacSoftware/foo --output-file ./foo.pkg
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

## Agents

```sh
stemma mcp
```

`mcp` serves the catalog's tools to an agent over the Model Context Protocol on
standard input and output. `describe` answers what documents can declare, from
the built-in operations and the catalog's plugins; `prepare` tries documents
against their current sources without the lockfile and reports the inspected
artifact and its signer; `update`, `icon` and `check` match the commands. No
tool contacts a destination.

## Inspection and configuration

```sh
stemma inspect installer.pkg
stemma validate --resolved
stemma schema --output-file stemma.schema.json
stemma schema --offline --output-file -
stemma version
```

`validate --resolved` and `schema --output-file -` print JSON documents.
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
stemma plugins update [NAME...]
stemma plugins publish IMAGE --goreleaser dist
stemma cache path
stemma cache info
stemma cache prune --dry-run
stemma cache prune
stemma cache prune --all
```

`plugins list` loads each plugin from its lock entry and describes what it runs
and offers. `plugins update` locks the named plugins, or all of them, to what their
declarations select now: it resolves a tag again and snapshots local files.
`update` locks the plugins whose entries are missing or stale; no other command
changes plugin entries. See [using plugins](plugins.md).

`plugins publish` pushes a GoReleaser release of plugin bundles as an OCI platform
index; see [creating plugins](writing-plugins.md#distribute-a-bundle).

The disposable cache defaults to the system's user cache directory under `stemma`.
`--cache-dir` / `STEMMA_CACHE_DIR` overrides it; `cache path` prints its location.
`reconcile --state-dir` / `STEMMA_STATE_DIR` relocates the applied marker from
`.stemma/state` under the project root. See [reconciliation state](reconcile.md#cache-and-state).
`--cache-max-size` / `STEMMA_CACHE_MAX_SIZE` sets the soft retained-cache budget
(default `32GiB`, `0` disables automatic content eviction). Normal pruning
protects content used in the last 24 hours; `--all` overrides that protection.
`cache info` and `cache prune` accept `--json`. Pruning reports reclaimed bytes
and never removes published packages, reconciliation state or user exports.
See [cache and offline runs](sources.md#cache-and-offline-runs) for lifecycle and
accounting details.

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

Do not infer success solely from an artifact appearing in a report. Update records
the inputs of successful resources even when it exits nonzero; failed or blocked
resources keep their reviewed entries. An update report without `lock_changed`
did not complete the lockfile comparison.

## Standalone packaging

```sh
stemma package pkg --help
stemma package intunewin --help
```

These expose low-level packaging for direct use. Catalogs normally use
`BuildMacPkg` or the Intune destination instead. `stemma <command> --help` is the
reference for the flags supported by your installed binary.
