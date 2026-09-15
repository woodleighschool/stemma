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

- `validate` reads the authored spec and returns named inputs, preparation-only
  configuration and destination settings.
- `run` receives locked inputs in a leased workspace and returns named artifacts.

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
	Source       plugin.Input              `json:"source"`
	Destinations map[string]map[string]any `json:"destinations"`
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
	err := registry.Register(plugin.Operation{
		Name:        "catalog-tools.package",
		Kind:        "resource",
		Resource:    &plugin.ResourceKind{APIVersion: "example.org/v1", Kind: "VendorPackage"},
		SideEffects: "workspace",
		Methods:     []string{"validate", "run"},
		ConfigSchema: json.RawMessage(`{
			"type": "object",
			"required": ["source", "destinations"],
			"additionalProperties": false,
			"properties": {
				"source": {"type": "object"},
				"destinations": {"type": "object"}
			}
		}`),
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, prepare)
	if err != nil {
		return err
	}
	return plugin.Serve(ctx, os.Stdin, os.Stdout, registry)
}

func prepare(ctx context.Context, envelope plugin.Request) (plugin.Response, error) {
	var request plugin.ResourceRequest
	if err := json.Unmarshal(envelope.Input, &request); err != nil {
		return plugin.Response{}, err
	}
	var result plugin.ResourceResult
	switch envelope.Method {
	case "validate":
		var config spec
		if err := json.Unmarshal(request.Config, &config); err != nil {
			return plugin.Response{}, err
		}
		result.Inputs = map[string]plugin.Input{"source": config.Source}
		result.Config = json.RawMessage(`{}`)
		result.Destinations = config.Destinations
	case "run":
		artifact := request.Inputs["source"]
		if artifact.Tree || !strings.EqualFold(filepath.Ext(artifact.Filename), ".pkg") {
			return plugin.Response{}, errors.New("source must be a PKG file")
		}
		artifact.Format = "pkg"
		result.Artifacts = map[string]plugin.Artifact{"installer": artifact}
		plugin.Logger(ctx).InfoContext(ctx, "Prepared vendor package")
	}
	output, err := json.Marshal(result)
	return plugin.Response{Output: output}, err
}
```

Initialise and build the module:

```sh
go mod init example.org/catalog-tools
go get github.com/woodleighschool/stemma@main
go build -o plugin .
```

On Windows, name the output `plugin.exe`. `go get` records the resolved revision in
`go.mod`; keep that pin. The example's envelope schemas only require objects to
keep the entry point small. A published plugin should describe its supported
request and response fields with self-contained JSON Schemas.

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

`ResolveRequest` supplies declaration `config`, resource-relative `base`, project
`root`, a workspace, `locked` and the previous `observation`. Return a
`ResolveResponse` containing the observation and artifact.

When `locked` is false, discover the input. When true, reproduce the recorded
observation instead of asking for the latest release. Stemma owns the surrounding
versioned lock, declaration fingerprint and content verification; your resolver
owns the observation body. Keep credentials out of observations and identify
credential configuration fields with `writeOnly: true`.

## Destinations

Register `kind: reconcile` with methods `validate`, `plan` and `apply`. Use
`ConfigSchema` for connection settings and `MetadataSchema` for authored native
settings. `ReconcileRequest` includes logical identity, primary artifact, named
artifact inputs, facts, subject selectors, the prior binding and peer bindings.

`plan` reads state and returns semantic `Change` records without mutations.
`apply` re-observes, performs the necessary changes and returns a durable binding.
Preserve absent, null, false and empty collection values when decoding native
metadata; they have different meanings.

The binding is provider-owned JSON. Store native IDs and enough ownership evidence
to recover an interrupted write. An omitted response binding preserves the old
one; JSON null clears it; a value replaces it. Return partial output on failure
when an owned object was created or an upload needs recovery. The host preserves
returned bindings even alongside an error.

Do not infer ownership from a display name. Re-running identical desired content
must not duplicate objects or repeat uploads. Use the shared `Publications` and
retention types for successful payload order, while implementing native reference
checks and cleanup in the provider. See [retention](publishing.md#identity-and-retention).

## Protocol and runtime

The host launches an executable for one request. Protocol version **3** sends one
JSON object on stdin, ending at EOF:

```json
{
  "protocol": 3,
  "method": "describe"
}
```

The final stdout response has `protocol`, optional `output` and optional `error`.
`describe` returns a `Descriptor` containing provider name, version and operations.
Other requests add `operation`, `input` and optionally `log_level`.

Before the final response, a plugin may emit newline-delimited envelopes containing
`protocol: 3` and `log`, a structured record with time, level and message. Messages
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

Local files are a first-class distribution option. For OCI distribution, create
one tar.zst bundle per runner platform with `plugin` or `plugin.exe` and its helper
files at the archive root. Use:

| OCI field               | Value                                              |
| ----------------------- | -------------------------------------------------- |
| Artifact type           | `application/vnd.stemma.plugin.v1`                 |
| Bundle layer media type | `application/vnd.stemma.plugin.bundle.v1.tar+zstd` |
| Index platform          | The runner's OS and architecture                   |

Combine platform manifests in an OCI index and publish it with a tag or digest.
Stemma pins the index and fetches only the selected runner's bundle. All code runs
with the caller's privileges; the protocol is not a sandbox.
