# Writing plugins

A plugin is an executable speaking Stemma's JSON protocol. It can register a
resource kind, source resolver or destination. The public
[Go SDK](https://github.com/woodleighschool/stemma/tree/main/plugin) supplies the
registry, request types and protocol framing used by built-ins. Other languages
can implement the same wire contract.

Pin the SDK revision in your plugin's dependencies and test it with the Stemma
version your catalog uses. The interface is still under development.

## Register a resource kind

A resource has two methods:

- `discover` reads the declared spec and returns named inputs, preparation-only
  configuration and destination settings.
- `run` receives concrete preparation configuration and locked inputs in a leased
  workspace, and returns named artifacts.

Separating destination settings from preparation configuration lets a metadata
edit reuse the existing artifact. Resolve downloads through inputs; do not hide
upstream discovery inside `run`.

This minimal `VendorPackage` kind passes a resolved PKG through to a destination.
It demonstrates input resolution and content handoff without constructing another
package. Save it as `main.go` in a plugin module:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

type spec struct {
	Source       plugin.Input              `json:"source" jsonschema_description:"Vendor installer to acquire."`
	Destinations map[string]map[string]any `json:"destinations" jsonschema_description:"Native metadata for each named destination."`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(ctx context.Context) error {
	registry := plugin.New("catalog-tools", "1.0.0")
	err := plugin.Register(registry, plugin.Operation{
		Name:        "catalog-tools.package",
		Kind:        "resource",
		Resource:    &plugin.ResourceKind{APIVersion: "example.org/v1", Kind: "VendorPackage"},
		SideEffects: "workspace",
		Methods:     []string{"discover", "run"},
	}, prepare)
	if err != nil {
		return err
	}
	return plugin.Serve(ctx, os.Stdin, os.Stdout, registry)
}

func prepare(ctx context.Context, request plugin.ResourceRequest[spec]) (plugin.ResourceResult, error) {
	var result plugin.ResourceResult
	switch request.Method {
	case "discover":
		result.Inputs = map[string]plugin.Input{"source": request.Config.Source}
		result.Config = json.RawMessage(`{}`)
		result.Destinations = request.Config.Destinations
	case "run":
		artifact := request.Inputs["source"]
		if artifact.Tree || !strings.EqualFold(filepath.Ext(artifact.Filename), ".pkg") {
			return result, errors.New("source must be a PKG file")
		}
		artifact.Format = "pkg"
		result.Artifacts = map[string]plugin.Artifact{"installer": artifact}
		plugin.Logger(ctx).InfoContext(ctx, "Prepared vendor package")
	}
	return result, nil
}
```

Initialise and build the module:

```sh
go mod init example.org/catalog-tools
go get github.com/woodleighschool/stemma@main
go build -o plugin .
```

On Windows, name the output `plugin.exe`. `go get` records the resolved revision in
`go.mod`; keep that pin. `plugin.Register` derives the request, config and response
schemas from the handler types. The protocol and catalog editor use those same
contracts.

## Typed configuration

Use Go fields for structure, `omitempty` for optional fields, and invopop's
`jsonschema` tags for constraints and defaults. Add `jsonschema_description` to
fields whose purpose or behaviour benefits from an editor hover.

```go
type Architecture string

const (
	ARM64 Architecture = "arm64"
	X64   Architecture = "x64"
)

func (Architecture) JSONSchemaExtend(s *jsonschema.Schema) {
	s.Enum = []any{ARM64, X64}
}

type Config struct {
	Major int `json:"major" jsonschema:"minimum=1" jsonschema_description:"Major release to track."`
	Architecture Architecture `json:"architecture,omitempty" jsonschema:"default=arm64" jsonschema_description:"Installer CPU architecture."`
}
```

Import `github.com/invopop/jsonschema` for the enum hook. Registration rejects
unknown fields, checks required fields and enum values, applies schema defaults,
then decodes the effective config. An optional `Validate() error` method handles
semantic rules before a resolver or destination handler runs, including locked
requests. Resource discovery validates the authored declaration; runtime values
must satisfy the concrete preparation contract before use. Expression-bearing
fields are checked again after evaluation. `run` receives preparation config,
without source declarations or destination metadata. Resolver `validate` requests
stop after configuration checks and perform no acquisition. Direct calls to typed
resolvers supply effective config values.

Defaults live in tags once; omitted values receive them while explicit zero,
false and empty values remain explicit. Required fields must be supplied even
when they have a default annotation. Sparse destination metadata retains its
absent/null/value semantics and does not receive config defaults.

JSON Schema describes structural constraints. Cross-field and external-system
rules may remain authoritative in Go; use `JSONSchemaExtend` only when a small
schema addition helps editors. `stemma schema --output-file stemma.schema.json` composes the
static document structure with every locally loaded operation. No copy of a
plugin's config fields belongs in the catalog schema generator.

## Run it from a catalog

Copy the executable into `plugins/catalog-tools/` in your catalog and add this
Project entry:

```yaml
spec:
  plugins:
    catalog-tools:
      path: plugins/catalog-tools
      trusted: true
```

With the `munki` connection from [getting started](getting-started.md), add a
resource and a real vendor package at `Assets/Vendor.pkg` beside its document:

```yaml
apiVersion: example.org/v1
kind: VendorPackage
metadata:
  name: vendor-app
spec:
  source:
    path: Assets/Vendor.pkg
  destinations:
    munki:
      pkginfo:
        description: Vendor application.
```

```sh
stemma validate
stemma prepare VendorPackage/vendor-app
stemma plan VendorPackage/vendor-app
```

The native Munki destination accepts the PKG and inspects it through the ordinary
artifact path. It does not need to know `VendorPackage` exists. A consumer can also
reference this resource's `installer` output using its full `apiVersion`, `kind`,
name and output.

## Artifact and evidence handoff

`plugin.Artifact` describes a file or tree with its path, filename, format, content
digest and size. It can include an entry point, selected version, typed `Facts`
and namespaced JSON `Evidence`.

Create new outputs inside `request.Workspace`. Treat leased inputs as immutable;
the host recomputes identity, checks declared hashes and detects input mutation.
Do not retain workspace paths between requests or reach into the host's cache.
Plugin bundle identity participates in preparation cache keys.

Typed facts retain separate subjects: a package receipt, contained application or
MSI can each have its own identity and version. Paths describe artifact provenance;
installed paths describe endpoint locations. Optional evidence such as
`com.example.inspection` can carry additional JSON without extending a closed core
model. Consumers must understand evidence before using it.

Destination descriptors use `ContentContract` to accept formats, trees and/or
source-free content. This is independent of originating kind names. A destination
must still validate its native deployment mode: accepting a PKG does not mean it
accepts Munki `nopkg` or an arbitrary script policy.

## Resolvers

Register an operation with `kind: resolve`, methods `validate` and `run`, and a
`ResolverKind` containing the observation contract's version. Set `local: true`
when consuming a lock must also check current local files.

`ResolveRequest[Config]` supplies declaration `config`, resource-relative `base`, project
`root`, a workspace, `locked` and the previous `observation`. Return a
`ResolveResponse` containing the observation and artifact.

When `locked` is false, discover the input. When true, reproduce the recorded
observation instead of asking for the latest release. Stemma owns the surrounding
versioned lock, declaration fingerprint and content verification; your resolver
owns the observation body. Keep credentials out of observations and identify
credential configuration fields with `writeOnly: true`.

Return consumer metadata in namespaced `artifact.evidence`, for example
`{"vendor.release":{"version":"1.2"}}`. Evidence is reviewed in the source lock
and passed to resource inputs; destination metadata can reference it with
`{{ evidence['vendor.release'].version }}`. A builder reads the same evidence
through `inputs.<name>.evidence`. Changing evidence invalidates preparation even
when the bytes are unchanged. The byte timestamp stays the same.

Observation remains private to the resolver. A locked fetch verifies bytes and
uses the lock's saved evidence, ignoring evidence returned by the fetch. Resolver
artifact versions, formats and typed facts are not persisted; resource kinds own
their interpretation of the downloaded content. Keep credentials and temporary
URLs out of evidence as well as observations.

## Destinations

Register `kind: reconcile` with methods `validate`, `plan` and `apply`. Use
`ConfigSchema` for connection settings and `MetadataSchema` for native
settings. `ReconcileRequest[Config]` includes logical identity, the primary
artifact with its facts and managed `Version`, named artifact inputs, peers and,
for macOS software, `MinimumOS`: the latest of the installer's requirement, the
selected application's and the software's `minimum_os`, with the origin of the
value that won.

`plan` reads the destination and returns semantic `Change` records without
mutations. `apply` re-observes and performs the necessary changes. Preserve absent,
null, false and empty collection values when decoding native metadata; they have
different meanings. Return the changes already made alongside an error.

Identify publications from native keys or a marker in a remote field. Re-running
with an empty local cache must find existing objects and skip completed uploads.
After an interruption, re-read the destination before writing again.

Explicit fields override derived values. Missing derived values clear owned
fields; other omitted fields remain unchanged. Ownership follows the current
declaration and artifact.
Within destination metadata, `resource` is reserved for an explicit
`ResourceReference` containing `apiVersion`, `kind` and `name`. A destination's
metadata schema defines where that reference is accepted and which native
relationship properties may accompany it. Stemma reads these references directly,
validates the publication graph, and reconciles selected peers first.

`Identity.Resource` identifies the current resource. `Peers` maps each referenced
resource's `ResourceReference.Key()` to its declared metadata for this connection,
including unselected peers. The key defaults the API version to `stemma/v1alpha1`.
Resolve native IDs from that metadata and the resource identity; missing peers are
errors. Never reinterpret resource names as native strings or remote search terms.
`ReconcileResponse` reports changes and origins; it does not discover dependencies.

`ResourceOutputReference` adds an output selector to resource identity for immutable
preparation inputs. It is distinct from publication references, which have no
output selector. See [the two graphs](catalogs.md#two-dependency-graphs).

Use the shared `Retention` type and order a software's publications from the
destination's own records, while implementing native reference checks and cleanup in
the provider. See [retention](publishing.md#identity-and-retention).

## Protocol and runtime

The host launches an executable for one request. Protocol version **5** sends one
JSON object on stdin, ending at EOF:

```json
{
  "protocol": 7,
  "method": "describe"
}
```

The final stdout response has `protocol`, optional `output` and optional `error`.
`describe` returns a `Descriptor` containing provider name, version and operations.
Other requests add `operation`, `input` and optionally `log_level`.

Before the final response, a plugin may emit newline-delimited envelopes containing
`protocol: 7` and `log`, a structured record with time, level and message. Messages
are bounded to 4 MiB. No messages may follow the final response. The SDK's `Serve`
and `Run` handle framing and validation.

Reserve stdout for the protocol. Use `plugin.Logger(ctx)` for structured diagnostics
and `plugin.Stage(ctx, "Preparing installer")` for progress; call the returned
function with the operation error when the stage finishes. A stage started inside
another shows as that operation's current step. Raw subprocess stderr
is discarded by the host, so report operational failures through the protocol.
Do not log credentials or request bodies.

`Platforms` contains supported runner `GOOS/GOARCH` pairs; omission means portable.
`Requirements` describes commands, purpose and actionable setup instructions.
Declare dependencies on interpreters, signing tools or packaging helpers. Scripts
intended for installation remain endpoint payload, not runner commands.

## Distribute a bundle

Local files are a first-class distribution option. A published plugin is an OCI
platform index with one manifest per runner platform. Each manifest carries a
tar.zst bundle with `plugin` or `plugin.exe` and its helper files at the archive
root:

| OCI field               | Value                                              |
| ----------------------- | -------------------------------------------------- |
| Artifact type           | `application/vnd.stemma.plugin.v1`                 |
| Bundle layer media type | `application/vnd.stemma.plugin.bundle.v1.tar+zstd` |
| Index platform          | The runner's OS and architecture                   |

Stemma pins the index and fetches only the selected runner's bundle. All code runs
with the caller's privileges; the protocol is not a sandbox.

### Release a Go plugin

GoReleaser builds the bundles and Stemma publishes them. Build a `plugin`
executable for each runner platform and archive it as tar.zst:

```yaml
version: 2
builds:
  - binary: plugin
    env:
      - CGO_ENABLED=0
    goos: [darwin, linux, windows]
    goarch: [amd64, arm64]
archives:
  - formats: [tar.zst]
```

After GoReleaser runs, publish the archives it lists in `dist/artifacts.json`:

```sh
stemma plugins publish ghcr.io/example/catalog-tools:1.0.0 --goreleaser dist
```

`publish` checks each bundle the way the loader will, pushes a manifest for each
platform and tags their index last, so a failed run leaves the previous release in
place. Registry credentials come from `docker login`. Add index annotations with
`--annotation KEY=VALUE`, such as `org.opencontainers.image.source` for the
repository URL. In GitHub Actions, the
[publish-plugin action](https://github.com/woodleighschool/stemma/tree/main/actions/publish-plugin)
runs this step after GoReleaser.
