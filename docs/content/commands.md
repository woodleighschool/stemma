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

| Command                           | Purpose                                                                 |
| --------------------------------- | ----------------------------------------------------------------------- |
| `stemma validate`                 | Check the catalog as written, without environment values or acquisition |
| `stemma update [Kind/name...]`    | Discover current inputs and update their locks                          |
| `stemma prepare [Kind/name...]`   | Prepare resources from the lock without publication                     |
| `stemma signature [Kind/name...]` | Derive signed or unsigned expectations for each signing subject         |
| `stemma artifact Kind/name`       | Prepare one resource from the lock and print the path of its artifact   |
| `stemma icon [Kind/name...]`      | Create missing declared icons from the software's own artwork           |
| `stemma plan [Kind/name...]`      | Read destinations and report proposed changes                           |
| `stemma apply [Kind/name...]`     | Re-read and reconcile destinations once                                 |

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
other command uses the reviewed lock as it is: an input without a current
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
rest. The lock must hold entries for exactly the declared inputs of the
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

Without `--input`, `--path` names an application in one resource's prepared
installer, for a package that holds several and selects none:

```sh
stemma icon MacSoftware/suite --path "Payload/Example.app"
```

Use `--from PATH` to take the artwork from a local application, installer or
artwork file, for software that carries none of its own:

```sh
stemma icon MacSoftware/fonts --from "/System/Applications/Font Book.app"
```

It writes the icon of one resource that declares `spec.icon`. The resource needs
no source or lock entry, and `--path` selects within the local source as it does
within an input.

Existing icons stay unchanged unless `--force` is set. Only resources needing an
icon are prepared. The command leaves the lock unchanged and does not contact
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

`--read PATH` prints one file inside the artifact instead of its path. The cached
artifact is read where it is; nothing is copied out. Inside a
package, name an entry such as `Distribution`, or a file in a component's
`Scripts` or `Payload` archive, which is decoded up to that file. Inside a disk
image, archive or folder, name a path; it continues into a package found there. A
directory prints the names it holds, and `.` names the root. The path stays inside
the artifact: it takes no absolute or parent path and follows no symlink. A
redirect or pipe receives the file's bytes as they are. Terminal output rejects
NUL and escape bytes but does not require UTF-8:

```sh
stemma artifact MacSoftware/foo --read .
stemma artifact MacSoftware/foo --read Distribution
stemma artifact MacSoftware/foo --read Foo.pkg/Scripts/postinstall
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
against their current sources without the lock and reports the inspected
artifact, its signer and each destination's `publications`; `update`, `icon` and `check` match the commands. A
relative `from` given to `icon` starts at the catalog root. No tool contacts a
destination.

## Inspection and configuration

```sh
stemma inspect installer.pkg
stemma validate
stemma schema --output-file stemma.schema.json
stemma schema --offline --output-file -
stemma version
```

`schema --output-file -` prints a JSON document.
`schema` requires an explicit output file and describes built-in and loaded plugin
contracts independently of configured destination connections. `validate` checks
destination references and metadata against the configured operations.
`--builtins` generates the default schema without loading a catalog; it uses the
same registry and schema composition as project generation.
`inspect` inventories a local file, directory or archive without executing it or
loading a project. `--json` prints its facts; `--path` filters by subject path. Pass it
the path `artifact` prints to inspect what Stemma prepares for a resource.
`--read PATH` prints one file inside it instead, as `artifact --read` does.

To inspect a named input before its build expressions can resolve, use:

```sh
stemma inspect BuildMacPkg/example --input vendor --path Installer.app --json
```

This reads the locked input without building the selected resource. Omit `--path`
to inventory the source; add `--no-input-lock` to inspect current sources without
writing the lock. MCP `inspect` takes `resource`, `input` and an optional `path`
and uses current sources. Both return the subject IDs and static facts used by
`inputs.<name>.facts` expressions. `--read PATH`, or MCP `read`, returns one file
inside the input instead; MCP returns UTF-8 text up to 256 KiB, including control
characters escaped by JSON. Selecting a package inside an archive returns
its inventory entry. Inspect the package itself to read its receipts and payload.
`validate` reads no environment values, lock entries or input content; the
commands that use them check them. It loads the project's plugins, from the cache
with `--offline`.

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
stemma cache verify
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
Cached content is checked as it is stored and trusted afterwards. `cache verify`
reads every cached object, discards any whose bytes no longer match, and fails
when it discarded one; it also accepts `--json`.
See [cache and offline runs](sources.md#cache-and-offline-runs) for lifecycle and
accounting details.

`--jobs` / `STEMMA_JOBS` limits concurrent preparations (default `2`, `1` runs
serially). Independent resources can prepare together once their producers
finish. Input acquisition and destination operations remain serial. Workers
release their workspaces after storing outputs, and cancellation waits for
workers before releasing the cache lease. Artifact, inspection and icon commands
prepare only the outputs they need, serially.

## Reports and diagnostics

Outcome commands (`update`, `prepare`, `signature`, `icon`, `plan`, `apply`)
write a block to stdout for each resource as it finishes, then a summary line:

```text
✓ MacSoftware/example:
  munki: 1.0 → 2.0
    pkginfo.description: Original description → Revised description
    upload installer: example-2.0.pkg
      digest: aaaaaaaaaaaa… → bbbbbbbbbbbb…
    remove 1.0
✓ MacSoftware/current: unchanged (intune 3.1.1)
✗ MacSoftware/another:
  munki: 1.0 unchanged
  intune: failed
    upload failed

3 resources: 1 published, 1 unchanged, 1 failed; 1 older version removed.
```

Headings use `name: summary`. Children are indented two spaces beneath their
owner: resources contain named inputs or destinations, which contain changes and
supporting values. A result without a separate subject belongs to the resource
heading. Long values move beneath their field label; wrapped lines keep their
indentation. Terminal output fits the available width; plain output uses 100
columns. Resources follow each other without blank lines, including unchanged
resources, which use one line each.

- `update` shows a source's version change, or its filename when unversioned.
  New bytes under the same version read `file changed`. Every input change
  includes the changed lock fields. Changed plugin pins and removed lock documents
  follow the resources.
- `plan` is a dry run of `apply`: both show the same version transitions,
  field changes with their before and after values, uploads and removals.
  Initial fields show their values. The summary distinguishes proposed
  publications from completed ones. Jamf uses package filenames as its native
  publication identity.
- `prepare` shows the installer's version and file, `signature` the signers and
  fragment to declare, and `icon` the asset it created.

Collections use `-` and `+`; receipts, installs and included apps match by
identity. Multi-line values such as scripts show line diffs. Human and JSON
reports include field values; pull requests omit them.

Every selected resource appears, including unchanged and skipped resources.
A tick marks a successful resource; a cross marks a failed or blocked resource.
A failure at any destination fails the resource, while its other destination
results remain visible. Skipped resources use a dash and state the reason.
Error explanations appear beneath the failed resource or destination.
The resource title carries the failure mark; error rows do not repeat it.
The summary counts every selected resource by outcome.

Upload filenames and digests, changed lock fields and additional artifact
outputs appear in the standard report.

Colour reinforces the text, which reads the same without it: new values are
green, earlier values and unchanged resources subdued, changes worth a second
look yellow, failures red and blocked outcomes yellow.
Collection members added and removed show `+` and `-`.
Active resource titles are bold magenta. Completed titles, destination names,
field labels and progress text use the terminal's normal colour. The result mark
carries the success or failure colour.

`--json` writes one document holding every selected resource: its `status`
(`changed`, `unchanged`, `failed`, `blocked`, or `skipped` with a `reason`), the
lock `inputs` it changed (`kind` `added`, `removed`, `updated` or `refreshed`,
with the entries before and after), its `artifacts` and its `publications`. A
publication carries the preview preparation makes without connecting:
`metadata` with the declared and derived values the destination manages,
`origins` naming where each value came from (`explicit`, `expression`,
`artifact.version`, `installer.receipts`…), and `deferred` when no preview is
possible. Plan and apply add `version`, `previous`, `uploads`, `fields`,
`repairs` (derived destination records rebuilt), `removed` and `error`.
`summary` has the same keys for every command; update adds `lock_changed`.
Update saves each resource's inputs before streaming its result. The JSON document
includes warnings from command cleanup; startup failures can stop before a report
exists.

```sh
stemma plan --json > plan.json
stemma prepare MacSoftware/foo --json | jq '.resources[].publications'
```

Reconcile streams the reviewed commit's publication and then each proposal.
Pull request links and summaries appear beneath their outcome. Plugin reports,
cache reports and artifact inspection use the same heading and indentation rules;
inspection nests subjects beneath the artifact or package that contains them.

When stderr is a suitable terminal, only resources with running operations
appear in progress. The live display owns terminal output, including completed
results entering scrollback. Redirected reports still go to stdout. Prepared
resources waiting for publication stay offscreen.
Operations sit beneath their resource or named input or destination. Nested work
shows its current operation instead of repeating its parent loader; the primary
source needs no input label. Operations over a collection nest their items beneath
one label, including when there is only one item. Finished operations remain
ticked while later work runs. The tree collapses into the resource's result when
it finishes. Cache hits can mark an operation
`(cached)`; cache lookups, misses and bookkeeping do not add progress rows.
Progress bars sit below their operation labels. Known totals show counts and
percentages; unknown totals show a moving bar and the observed count. Operations
without measurable progress show a spinner. On short terminals, completed
operations leave the live region first to keep ongoing work visible.

Outside a terminal, long operations emit a progress line after two seconds and
at most every thirty seconds afterwards. Results use stdout. JSON and generated
documents suppress progress. `--no-progress`
disables both live and plain progress; `NO_COLOR` controls colour.
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
Failed commands retain available results and exit nonzero. Failures already
shown in the report are not repeated on stderr; startup and command-wide errors
go to stderr.
Interrupting with Ctrl-C clears activity, preserves observed publication results
and exits 130. A resource interrupted between destinations retains the outcomes
it received and is counted as failed.
Press Ctrl-C again to exit immediately if cleanup or a native operation is
taking too long.

Acquisition, preparation and destination failures are local: independent resources
continue, and consumers of unavailable resource outputs are reported as `blocked`
with `blocked_by` resource keys in JSON. The command emits the selected results
before exiting nonzero. Global configuration or structural failures and context
cancellation stop the run immediately and can leave a partial report.

Do not infer success solely from an artifact appearing in a report. Update retains
completed saves if a later resource fails or the run stops. A save failure stops
the run and reports any confirmed changes. An update report without `lock_changed`
did not reach a lock comparison.

## Standalone packaging

```sh
stemma package pkg --help
stemma package intunewin --help
```

These expose low-level packaging for direct use. Catalogs normally use
`BuildMacPkg` or the Intune destination instead. `stemma <command> --help` is the
reference for the flags supported by your installed binary.
