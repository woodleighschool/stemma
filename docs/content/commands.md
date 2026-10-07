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
ambiguous identity. Omitting selectors processes every resource that declares no
[profile](catalogs.md#group-opt-in-work-in-profiles) and is not
[suspended](catalogs.md#suspend-a-resource). `--profile NAME` processes the
resources in that profile instead; repeat it to add profiles. A selector runs
the resource it names, profiled or suspended, and the builds it references.
`--profile` and selectors cannot be combined.

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
the resources that consume it. The command compares the resources a run without
selectors takes: suspended resources stay out, and profiled resources are
compared only with `--profile`, which compares that profile in place of the
rest. The lockfile must hold entries for exactly the declared inputs of the
compared resources and the builds they reference, and none for resources the
catalog no longer declares; entry and environment values are read only for the
resources the command prepares. Each catalog is interpreted by
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

Use `--input NAME` to read a locked input of the selected resource or its build
dependencies, with `--path` to select an application, installer or artwork file.
The selected resource must declare `spec.icon`, which names the asset:

```sh
stemma icon MacSoftware/example --input vendor --path Installer.app
```

Input names inherited from build dependencies must be unambiguous. Only an input
that references a resource output requires preparation.

Existing icons stay unchanged unless `--force` is set. Only resources needing an
icon are prepared. The command leaves the lockfile unchanged and does not contact
destinations. Commit the icons so publication sends the same bytes on every host.

`signature` acquires and prepares inputs like `prepare`, records each subject's
signed or unsigned state and prints a complete `signatures` fragment. It never
writes documents and ignores existing signing expectations while deriving their
replacement. Invalid or unsupported signatures still fail. For `BuildMacPkg`, the
command reports the signing subjects its resolved layout consumes, labelled with
their input. It reports nothing for the package a build produced, which needs no
expectation. See [macOS](mac-software.md#signature),
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
stderr, and the live display needs only stderr to be a terminal, so the command
composes with other tools:

```sh
stemma inspect "$(stemma artifact MacSoftware/foo)"
stemma inspect "$(stemma artifact MacSoftware/foo --no-input-lock)"
pkgutil --check-signature "$(stemma artifact MacSoftware/foo)"
stemma artifact MacSoftware/foo --output-file ./foo.pkg
```

`--offline` requires cached network inputs and plugins; destination calls
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
the built-in operations and the catalog's plugins; `inspect` reads a named input's
static facts before a build; `prepare` tries documents
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
`inspect` inventories a local file, directory or archive without executing it or
loading a project. `--json` prints its facts; `--path` filters by subject path. Pass it
the path `artifact` prints to inspect what Stemma prepares for a resource.

To inspect a named input before its build expressions can resolve, use:

```sh
stemma inspect BuildMacPkg/example --input vendor --path Installer.app --json
```

This reads the locked input without building the selected resource. Omit `--path`
to inventory the source; add `--no-input-lock` to inspect current sources without
writing the lockfile. MCP `inspect` takes `resource`, `input` and an optional `path`
and uses current sources. Both return the subject IDs and static facts used by
`inputs.<name>.facts` expressions. Selecting a package inside an archive returns
its inventory entry. Inspect the package itself to read its receipts and payload.
`validate` needs no credentials. `validate --resolved` also evaluates every
environment value, as runs do, and prints the merged configuration with connection
settings resolved. It needs every referenced variable and may expose secrets: do
not share it without reviewing it.

## Plugins and cache

```sh
stemma plugins list
stemma plugins inspect
stemma plugins inspect --verify
stemma plugins update [NAME...]
stemma plugins publish IMAGE --goreleaser dist
stemma cache path
stemma cache info
stemma cache prune --dry-run
stemma cache prune
stemma cache prune --all
```

`plugins list` shows declarations and recorded pins without acquiring or running
plugins. `plugins inspect` loads the pinned code and reports its capabilities;
`--verify` also compares each installed plugin with its pinned code. `plugins update` locks the named plugins, or all of them, to what their
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

Outcome commands name their work, write each resource's report to stdout as it
finishes, then the run's totals. Plan shows proposed changes, including initial values for new
objects. Apply reports confirmed publication actions. Update shows input changes,
prepare keeps a completion line for every resource, icon names the artwork file it writes,
and signature shows derived signers. Multi-line values such as scripts show line
counts instead of their text. Explicitly selected resources are shown even when
unchanged. `--all` includes unchanged resources in human
reports. Preparation shows filenames for newly prepared artifacts; cached resources
use one line unless explicitly selected or expanded with `--all`. Totals always
describe the whole run. JSON includes all resources in the
selected scope, regardless of `--all`. Reconcile prints the reviewed commit's
publication followed by proposal outcomes, under separate applying and update-checking
sections.

```sh
stemma plan
stemma plan --json --all > plan.json
stemma prepare --all
```

Human reports use `➤` for headings, `✓` for successful or unchanged outcomes,
`✗` for failures and `–` for skipped or blocked work. Notices use `!` for warnings
and `i` for information. `→` marks proposed actions, before/after changes,
redirects and publication destinations. Versions follow names in parentheses;
statuses and field values follow a colon. Details indent by two spaces per level,
with one space between parts of a line. Blank lines separate sections; compact
outcomes stay adjacent. Colour
reinforces these markers; the same text remains readable without it. Diff lines
retain `+` and `-`, and literal payloads, YAML fragments and machine output retain
their own syntax.

When stderr is a suitable terminal, a live region shows the resource being worked
on: its heading, the step that is running, and above it the finished steps that
took a second or longer. Known-size transfers show a bar and counts. A step
that runs again continues its earlier line. Work that belongs to the command
rather than a resource shows on its own line below. A block taller than the
terminal scrolls into history instead of being cut. The resource's result
replaces its live block, and durable results have the same content in terminals
and pipes. Results remain on stdout even when it is redirected. JSON, generated
documents, CI and dumb terminals suppress the live display; `--no-progress`
disables it explicitly. `NO_COLOR` controls colour.
Warnings go to stderr as they happen; in JSON mode they are part of the document.
Resource notices appear once per command, including for cached or unchanged
resources, and remain visible with `--no-progress`. JSON keeps them in each resource's
`notices` with a level, rule code, message and optional hint. A missing signature
expectation produces a recommendation to run `stemma signature`: on directly sourced
MacSoftware, on WindowsSoftware whose setup entry point is an MSI, EXE or DLL, and on
a BuildMacPkg input that supplies an application or package. Declared signers,
explicit unsigned expectations and resource-built outputs do not.
A verified Developer ID application signature without a secure timestamp produces a
warning and stays valid.
Failed commands retain available results and end with a concise stderr diagnostic.
Interrupting with Ctrl-C clears activity, preserves completed results and exits 130.
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
