package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

type resourcesInput struct {
	Resources []string `json:"resources" jsonschema:"Resources as Kind/name, such as MacSoftware/firefox."`
}

type inspectInput struct {
	Resource string `json:"resource" jsonschema:"Resource as Kind/name."`
	Input    string `json:"input" jsonschema:"Declared input name, such as vendor."`
	Path     string `json:"path,omitempty" jsonschema:"Optional path within the input; omitted inventories the source."`
	Read     string `json:"read,omitempty" jsonschema:"Return this text file inside the input instead of its facts: a path in a disk image, archive or folder, continuing into a package as an entry such as Distribution or a file below a component's Scripts or Payload. A directory returns the names it holds, and . names the root."`
}

type inspection struct {
	Resource string             `json:"resource"`
	Input    string             `json:"input"`
	Artifact *engine.Inspection `json:"artifact,omitempty"`
	Content  *string            `json:"content,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// maxRead bounds the file a tool result carries.
const maxRead = 256 << 10

// textFile holds a file up to maxRead. Write is its only way in: a buffer's
// own ReadFrom would let a copy pass the bound.
type textFile struct{ data bytes.Buffer }

func (f *textFile) Write(p []byte) (int, error) {
	if f.data.Len()+len(p) > maxRead {
		return 0, fmt.Errorf("the file exceeds the %d KiB a tool result carries", maxRead>>10)
	}
	return f.data.Write(p)
}

func (c catalog) inspect(ctx context.Context, _ *mcp.CallToolRequest, in inspectInput) (*mcp.CallToolResult, inspection, error) {
	opts := c.options("inspect")
	opts.Resources = []string{in.Resource}
	opts.Input = engine.InputSelection{Name: in.Input, Path: in.Path}
	opts.Lock.IgnoreInputs = true
	var file textFile
	if in.Read != "" {
		opts.Read, opts.ReadTo = in.Read, &file
	}
	report, err := engine.Run(ctx, opts)
	if err == nil && !utf8.Valid(file.data.Bytes()) {
		err = errors.New("the file is not UTF-8 text")
	}
	result := inspection{Resource: in.Resource, Input: in.Input, Artifact: report.Inspection}
	if err != nil {
		result.Error = err.Error()
	} else if in.Read != "" {
		content := file.data.String()
		result.Content = &content
	}
	return outcome(err, result)
}

type iconInput struct {
	Resources []string `json:"resources" jsonschema:"Resources as Kind/name, such as MacSoftware/firefox."`
	Force     bool     `json:"force,omitempty" jsonschema:"Replace icons that already exist."`
	Input     string   `json:"input,omitempty" jsonschema:"Read a locked input of this resource or its build dependencies. The asset uses the selected resource's spec.icon."`
	From      string   `json:"from,omitempty" jsonschema:"Read this application, installer or artwork file instead, for one resource that declares spec.icon. A relative path starts at the catalog root."`
	Path      string   `json:"path,omitempty" jsonschema:"Application, installer or artwork path within the selected input or file. Alone, it names an application in one resource's prepared installer."`
}

type checkInput struct {
	Since string `json:"since" jsonschema:"Git revision the change is compared with, such as origin/main."`
}

// preparation answers prepare. Error carries a failure no resource reports.
type preparation struct {
	Resources []prepared `json:"resources"`
	Warnings  []string   `json:"warnings,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type prepared struct {
	Notices      []plugin.Notice `json:"notices,omitempty"`
	Resource     string          `json:"resource"`
	Error        string          `json:"error,omitempty"`
	BlockedBy    []string        `json:"blocked_by,omitempty"`
	Inputs       []input         `json:"inputs,omitempty"`
	Artifacts    []artifact      `json:"artifacts,omitempty"`
	Publications []publication   `json:"publications,omitempty"`
}

// publication is what one destination will publish for the artifact: the
// declared metadata with every derived value, and the origin of each field.
// Deferred says why Metadata is absent.
type publication struct {
	Destination string            `json:"destination"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Origins     map[string]string `json:"origins,omitempty"`
	Deferred    string            `json:"deferred,omitempty"`
}

// input is a source as the lock records it: the version and content it
// resolved to and the origin that fetches them. Kind compares it with the
// lock: added, removed, updated (new content) or refreshed.
type input struct {
	Input       string          `json:"input"`
	Kind        lock.ChangeKind `json:"kind"`
	Version     string          `json:"version,omitempty"`
	Filename    string          `json:"filename"`
	SHA256      string          `json:"sha256"`
	ContentRoot string          `json:"content_root,omitempty"`
	Resolver    string          `json:"resolver"`
	Origin      any             `json:"origin,omitempty"`
	Evidence    map[string]any  `json:"evidence,omitempty"`
}

// artifact is a prepared output with its inspected subjects. Signatures is the
// complete signed or unsigned expectations a document can declare.
type artifact struct {
	Output     string           `json:"output"`
	Filename   string           `json:"filename"`
	Format     string           `json:"format"`
	Version    string           `json:"version,omitempty"`
	Signatures string           `json:"signatures,omitempty"`
	Subjects   []plugin.Subject `json:"subjects,omitempty"`
	Evidence   map[string]any   `json:"evidence,omitempty"`
}

// lockUpdate answers update. Changed reports whether the lock was written.
type lockUpdate struct {
	Changed   bool      `json:"changed"`
	Resources []updated `json:"resources"`
	Plugins   []string  `json:"plugins,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// updated is a resource's update with its status as --json reports it.
type updated struct {
	Notices  []plugin.Notice `json:"notices,omitempty"`
	Resource string          `json:"resource"`
	Status   string          `json:"status"`
	Error    string          `json:"error,omitempty"`
	Inputs   []input         `json:"inputs,omitempty"`
}

// icons reports each resource's extraction outcome.
type icons struct {
	Resources []iconOutcome `json:"resources"`
	Error     string        `json:"error,omitempty"`
}

// iconOutcome is a resource's status as --json reports it, with the asset
// path the icon was created at or found, or why the resource has none.
type iconOutcome struct {
	Notices  []plugin.Notice `json:"notices,omitempty"`
	Resource string          `json:"resource"`
	Status   string          `json:"status"`
	Reason   string          `json:"reason,omitempty"`
	Icon     string          `json:"icon,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// checkResult answers check. Error carries a validation or lock failure.
type checkResult struct {
	Valid     bool      `json:"valid"`
	Resources []checked `json:"resources,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// checked is a resource the change prepared, with its status as --json
// reports it: changed (prepared), unchanged (cached), failed or blocked.
type checked struct {
	Notices   []plugin.Notice `json:"notices,omitempty"`
	Resource  string          `json:"resource"`
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	BlockedBy []string        `json:"blocked_by,omitempty"`
}

var errNoResources = errors.New("name at least one resource as Kind/name")

func (c catalog) prepare(ctx context.Context, _ *mcp.CallToolRequest, in resourcesInput) (*mcp.CallToolResult, preparation, error) {
	if len(in.Resources) == 0 {
		return nil, preparation{}, errNoResources
	}
	// Deriving signers verifies each artifact's signature without holding it
	// to a declared one, so a draft learns the signer it should declare.
	opts := c.options("signature")
	opts.Resources, opts.Lock.IgnoreInputs = in.Resources, true
	report, err := engine.Run(ctx, opts)
	result := preparation{Resources: []prepared{}, Warnings: report.Warnings, Error: unreported(err)}
	for _, resource := range report.Resources {
		item := prepared{Resource: resourceName(resource), Notices: resource.Notices, Error: resource.Error, BlockedBy: keyNames(resource.BlockedBy), Inputs: inputs(resource.Inputs)}
		for _, derived := range resource.Publications {
			published := publication{Destination: derived.Destination, Origins: derived.Origins, Deferred: derived.Deferred}
			if len(derived.Metadata) > 0 {
				if err := json.Unmarshal(derived.Metadata, &published.Metadata); err != nil {
					published.Deferred = "the destination reported metadata that is not an object"
				}
			}
			item.Publications = append(item.Publications, published)
		}
		for _, output := range slices.Sorted(maps.Keys(resource.Artifacts)) {
			item.Artifacts = append(item.Artifacts, describeArtifact(output, resource.Artifacts[output]))
		}
		result.Resources = append(result.Resources, item)
	}
	return outcome(err, result)
}

func (c catalog) update(ctx context.Context, _ *mcp.CallToolRequest, in resourcesInput) (*mcp.CallToolResult, lockUpdate, error) {
	if len(in.Resources) == 0 {
		return nil, lockUpdate{}, errNoResources
	}
	opts := c.options("update")
	opts.Resources = in.Resources
	report, err := engine.Run(ctx, opts)
	result := lockUpdate{Changed: report.LockChanged != nil && *report.LockChanged, Resources: []updated{}, Warnings: report.Warnings, Error: unreported(err)}
	for _, resource := range report.Resources {
		result.Resources = append(result.Resources, updated{Resource: resourceName(resource), Notices: resource.Notices, Status: string(resource.Status), Error: resource.Error, Inputs: inputs(resource.Inputs)})
	}
	for _, change := range report.Plugins {
		result.Plugins = append(result.Plugins, change.Name+": "+string(pluginChange(change)))
	}
	return outcome(err, result)
}

func (c catalog) icon(ctx context.Context, _ *mcp.CallToolRequest, in iconInput) (*mcp.CallToolResult, icons, error) {
	if len(in.Resources) == 0 {
		return nil, icons{}, errNoResources
	}
	opts := c.options("icon")
	opts.Resources = in.Resources
	opts.Input = engine.InputSelection{Name: in.Input, Path: in.Path}
	opts.Icons = engine.IconOptions{Presentation: icon.Auto, Size: icon.Size, Force: in.Force, From: in.From}
	if in.From != "" && !filepath.IsAbs(in.From) {
		// A client shares the catalog's files with the server, not its working directory.
		opts.Icons.From = filepath.Join(filepath.Dir(c.config), in.From)
	}
	report, err := engine.Run(ctx, opts)
	result := icons{Resources: []iconOutcome{}, Error: unreported(err)}
	for _, resource := range report.Resources {
		result.Resources = append(result.Resources, iconOutcome{Resource: resourceName(resource), Notices: resource.Notices, Status: string(resource.Status), Reason: resource.Reason, Icon: resource.Icon, Error: resource.Error})
	}
	return outcome(err, result)
}

func (c catalog) check(ctx context.Context, _ *mcp.CallToolRequest, in checkInput) (*mcp.CallToolResult, checkResult, error) {
	if in.Since == "" {
		return nil, checkResult{}, errors.New("since names the Git revision the change is compared with")
	}
	opts := c.options("prepare")
	opts.ChangedSince = in.Since
	report, err := engine.Run(ctx, opts)
	if err == nil {
		err = engine.ValidateProject(ctx, c.options(""))
	}
	result := checkResult{Valid: err == nil, Warnings: report.Warnings, Error: unreported(err)}
	for _, resource := range report.Resources {
		result.Resources = append(result.Resources, checked{Resource: resourceName(resource), Notices: resource.Notices, Status: string(resource.Status), Error: resource.Error, BlockedBy: keyNames(resource.BlockedBy)})
	}
	return outcome(err, result)
}

// outcome marks a run that failed as a tool error. Its report still answers
// with every resource's result; the SDK omits typed output from error
// results, so the report is attached here.
func outcome[Out any](err error, report Out) (*mcp.CallToolResult, Out, error) {
	if err == nil {
		return nil, report, nil
	}
	data, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		return nil, report, errors.Join(err, marshalErr)
	}
	return &mcp.CallToolResult{
		IsError:           true,
		StructuredContent: json.RawMessage(data),
		Content:           []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, report, nil
}

func unreported(err error) string {
	if err := engine.Unreported(err); err != nil {
		return err.Error()
	}
	return ""
}

func resourceName(resource engine.ResourceReport) string {
	return resource.Kind + "/" + resource.Name
}

// keyNames shortens resource keys, which include the API version, to
// Kind/name.
func keyNames(keys []string) []string {
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		parts := strings.Split(key, "/")
		names = append(names, strings.Join(parts[max(0, len(parts)-2):], "/"))
	}
	return names
}

func inputs(changes []lock.InputChange) []input {
	var result []input
	for _, change := range changes {
		entry := change.After
		if entry == nil {
			entry = change.Before
		}
		result = append(result, lockedInput(change.Input, change.Kind, *entry))
	}
	return result
}

func lockedInput(name string, kind lock.ChangeKind, entry source.Entry) input {
	result := input{Input: name, Kind: kind, Resolver: entry.Resolver, Filename: entry.Content.Filename, SHA256: entry.Content.SHA256, Evidence: decode(entry.Evidence), Version: entry.Version, ContentRoot: entry.Content.Root}
	if len(entry.Origin) > 0 {
		_ = json.Unmarshal(entry.Origin, &result.Origin)
	}
	return result
}

func describeArtifact(output string, prepared engine.Prepared) artifact {
	evidence := maps.Clone(prepared.Evidence)
	result := artifact{Output: output, Filename: prepared.Filename, Format: prepared.Format, Version: prepared.Version, Subjects: prepared.Facts.Subjects}
	if observations := signature.Read(evidence).Observed; observations != nil {
		result.Signatures = signature.Fragment(observations)
	}
	result.Evidence = decode(evidence)
	return result
}

func decode(evidence map[string]json.RawMessage) map[string]any {
	if len(evidence) == 0 {
		return nil
	}
	result := map[string]any{}
	for name, data := range evidence {
		var value any
		if json.Unmarshal(data, &value) == nil {
			result[name] = value
		}
	}
	return result
}

// pluginChange names a plugin pin change with the kinds input changes use.
func pluginChange(change lock.PluginChange) lock.ChangeKind {
	switch {
	case change.Before == nil:
		return lock.Added
	case change.After == nil:
		return lock.Removed
	}
	return lock.Updated
}
