// Package mcpserver serves one Stemma project to agents over the Model Context
// Protocol: what the project can declare, trial preparation, lock updates,
// icons and the checks pull requests run. It never contacts a destination.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/woodleighschool/stemma/internal/cas"
)

// Options locate the project a server works on.
type Options struct {
	ConfigPath  string
	CacheDir    string
	CachePolicy cas.Policy
	// Version names the Stemma build to clients.
	Version string
}

const instructions = `Stemma prepares the software a catalog declares from its vendors' sources and publishes it to destinations. These tools work on the catalog's files as they are now. describe answers what a document can declare; inspect reads a resource input's static facts; prepare tries documents against their current sources without the lockfile; update records sources in the lockfile; icon creates declared icons; check runs the checks a pull request runs. None of them publish.`

// Run serves the project's tools on standard input and output until the
// client disconnects or ctx ends.
func Run(ctx context.Context, opts Options) error {
	return newServer(opts).Run(ctx, &mcp.StdioTransport{})
}

func newServer(opts Options) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "stemma", Version: opts.Version}, &mcp.ServerOptions{Instructions: instructions})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			err = cas.Run(ctx, opts.CacheDir, opts.CachePolicy, false, func(ctx context.Context) error {
				var callErr error
				result, callErr = next(ctx, method, req)
				return callErr
			})
			return result, err
		}
	})
	tools := catalog{config: opts.ConfigPath, cache: opts.CacheDir}
	openWorld := true
	mcp.AddTool(server, &mcp.Tool{
		Name: "describe",
		Description: "Describe what this catalog can declare, from the installed Stemma and its plugins. " +
			"Without arguments: resource kinds, input resolvers with the values their fields accept, destinations and Project components. " +
			"Name a kind, resolver or destination for its fields, one line each with * marking required fields, and a field to narrow to one nested block. " +
			"A source names its resolver with resolver:, except that a url alone uses http and a path alone uses file.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, tools.describe)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "inspect",
		Description: "Inspect a resource input from its current source without building that resource or changing the lockfile. Returns static subjects and their exact paths, app versions and builds, identifiers, and installer facts. Use these facts to write build expressions.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld},
	}, tools.inspect)
	mcp.AddTool(server, &mcp.Tool{
		Name: "prepare",
		Description: "Prepare resources from their sources as they are now, ignoring the lockfile. " +
			"Reports the lockfile changes update would record, each artifact with its inspected facts, and signed or unsigned observations and the signatures block to declare. " +
			"Destination metadata is checked against the artifact. Writes nothing to the catalog.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld},
	}, tools.prepare)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update",
		Description: "Resolve the resources' sources and record them in the lockfile, reporting each input change. Other resources keep their entries.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &openWorld},
	}, tools.update)
	mcp.AddTool(server, &mcp.Tool{
		Name: "icon",
		Description: "Create each resource's declared icon, icons/<name>.png, from the artwork its locked software carries. " +
			"Set input and path to extract from a builder's vendor input. An existing icon stays unless force is set. macOS renders the glassy icon; other hosts normalize the raw artwork.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: &openWorld},
	}, tools.icon)
	mcp.AddTool(server, &mcp.Tool{
		Name: "check",
		Description: "Check the catalog as pull requests do: prepare affected resources " +
			"from the lockfile since a Git revision, then validate every document.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld},
	}, tools.check)
	return server
}

// catalog runs the tools against one project.
type catalog struct {
	config, cache string
}
