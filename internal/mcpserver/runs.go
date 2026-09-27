package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

type resourcesInput struct {
	Resources []string `json:"resources" jsonschema:"Resources as Kind/name, such as MacSoftware/firefox."`
}

type iconInput struct {
	Resources []string `json:"resources" jsonschema:"Resources as Kind/name, such as MacSoftware/firefox."`
	Force     bool     `json:"force,omitempty" jsonschema:"Replace icons that already exist."`
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
	Resource  string     `json:"resource"`
	Error     string     `json:"error,omitempty"`
	BlockedBy []string   `json:"blocked_by,omitempty"`
	Inputs    []input    `json:"inputs,omitempty"`
	Artifacts []artifact `json:"artifacts,omitempty"`
}

// input is a source as the lockfile records it: what its resolver observed and
// the content that fetched. Change compares it with the lockfile: added,
// removed, content changed or metadata refreshed.
type input struct {
	Name        string         `json:"name"`
	Change      string         `json:"change"`
	Resolver    string         `json:"resolver"`
	Observation any            `json:"observation,omitempty"`
	Filename    string         `json:"filename"`
	SHA256      string         `json:"sha256"`
	Size        int64          `json:"size"`
	Evidence    map[string]any `json:"evidence,omitempty"`
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

// lockUpdate answers update. Changed reports whether the lockfile was written.
type lockUpdate struct {
	Changed   bool      `json:"changed"`
	Resources []updated `json:"resources"`
	Plugins   []string  `json:"plugins,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type updated struct {
	Resource string  `json:"resource"`
	Error    string  `json:"error,omitempty"`
	Inputs   []input `json:"inputs,omitempty"`
}

// icons answers icon with each resource's outcome: created and the
// presentation, unchanged, no icon declared or no artwork.
type icons struct {
	Resources []iconOutcome `json:"resources"`
	Error     string        `json:"error,omitempty"`
}

type iconOutcome struct {
	Resource string `json:"resource"`
	Icon     string `json:"icon,omitempty"`
	Error    string `json:"error,omitempty"`
}

// checkResult answers check. Error carries a validation or lockfile failure.
type checkResult struct {
	Valid     bool      `json:"valid"`
	Resources []checked `json:"resources,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// checked is a resource the change prepared: prepared, cached, failed or
// blocked.
type checked struct {
	Resource  string   `json:"resource"`
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
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
		item := prepared{Resource: resourceName(resource), Error: resource.Error, BlockedBy: keyNames(resource.BlockedBy), Inputs: inputs(resource.Inputs)}
		for _, output := range slices.Sorted(maps.Keys(resource.Artifacts)) {
			item.Artifacts = append(item.Artifacts, describeArtifact(output, resource.Artifacts[output]))
		}
		result.Resources = append(result.Resources, item)
	}
	return outcome(err), result, nil
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
		result.Resources = append(result.Resources, updated{Resource: resourceName(resource), Error: resource.Error, Inputs: inputs(resource.Inputs)})
	}
	for _, change := range report.Plugins {
		result.Plugins = append(result.Plugins, change.Name+": "+pluginChange(change))
	}
	return outcome(err), result, nil
}

func (c catalog) icon(ctx context.Context, _ *mcp.CallToolRequest, in iconInput) (*mcp.CallToolResult, icons, error) {
	if len(in.Resources) == 0 {
		return nil, icons{}, errNoResources
	}
	opts := c.options("icon")
	opts.Resources = in.Resources
	opts.Icons = engine.IconOptions{Presentation: icon.Auto, Size: icon.Size, Force: in.Force}
	report, err := engine.Run(ctx, opts)
	result := icons{Resources: []iconOutcome{}, Error: unreported(err)}
	for _, resource := range report.Resources {
		result.Resources = append(result.Resources, iconOutcome{Resource: resourceName(resource), Icon: resource.Icon, Error: resource.Error})
	}
	return outcome(err), result, nil
}

func (c catalog) check(ctx context.Context, _ *mcp.CallToolRequest, in checkInput) (*mcp.CallToolResult, checkResult, error) {
	if in.Since == "" {
		return nil, checkResult{}, errors.New("since names the Git revision the change is compared with")
	}
	opts := c.options("prepare")
	opts.ChangedSince = in.Since
	report, err := engine.Run(ctx, opts)
	// Changed-since rejects plugin changes before any of their code runs.
	if err == nil {
		_, err = engine.ValidateProject(ctx, c.options(""), false)
	}
	result := checkResult{Valid: err == nil, Warnings: report.Warnings, Error: unreported(err)}
	for _, resource := range report.Resources {
		result.Resources = append(result.Resources, checked{Resource: resourceName(resource), Status: status(resource), Error: resource.Error, BlockedBy: keyNames(resource.BlockedBy)})
	}
	return outcome(err), result, nil
}

// outcome marks a run that failed as a tool error. Its report still answers
// with every resource's result.
func outcome(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: err != nil}
}

func unreported(err error) string {
	if err := engine.Unreported(err); err != nil {
		return err.Error()
	}
	return ""
}

func status(resource engine.ResourceReport) string {
	switch {
	case len(resource.BlockedBy) > 0:
		return "blocked"
	case resource.Error != "":
		return "failed"
	case resource.Cached:
		return "cached"
	}
	return "prepared"
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

func inputs(changes []lockfile.InputChange) []input {
	var result []input
	for _, change := range changes {
		entry, label := change.After, "metadata refreshed"
		switch {
		case change.Before == nil:
			label = "added"
		case change.After == nil:
			entry, label = change.Before, "removed"
		case change.ContentChanged:
			label = "content changed"
		}
		result = append(result, lockedInput(change.Input, label, *entry))
	}
	return result
}

func lockedInput(name, change string, entry source.Entry) input {
	result := input{Name: name, Change: change, Resolver: entry.Resolver, Filename: entry.Content.Filename, SHA256: entry.Content.Artifact.SHA256, Size: entry.Content.Artifact.Size, Evidence: decode(entry.Evidence)}
	var observation any
	if json.Unmarshal(entry.Observation, &observation) == nil {
		if object, ok := observation.(map[string]any); !ok || len(object) > 0 {
			result.Observation = observation
		}
	}
	return result
}

func describeArtifact(output string, prepared engine.Prepared) artifact {
	evidence := maps.Clone(prepared.Evidence)
	result := artifact{Output: output, Filename: prepared.Filename, Format: prepared.Format, Version: prepared.Version, Subjects: prepared.Facts.Subjects}
	var observations []signature.Observation
	if json.Unmarshal(evidence["signatures"], &observations) == nil {
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

func pluginChange(change lockfile.PluginChange) string {
	switch {
	case change.Before == nil:
		return "locked"
	case change.After == nil:
		return "removed"
	}
	return "changed"
}
