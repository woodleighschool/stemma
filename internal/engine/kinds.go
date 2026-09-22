package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/woodleighschool/stemma/internal/artifactname"
	"github.com/woodleighschool/stemma/internal/macpkg"
	"github.com/woodleighschool/stemma/internal/macsoftware"
	"github.com/woodleighschool/stemma/internal/windowssoftware"
	"github.com/woodleighschool/stemma/plugin"
)

func registerKinds(ops *operations) error {
	builder := resourceOperation("build.mac.pkg", "BuildMacPkg")
	builder.ConfigSchema = schemaJSON(plugin.SchemaFor[macpkg.Spec]())
	for _, err := range []error{
		plugin.Register(ops.registry, builder, buildMacPkg),
		plugin.Register(ops.registry, resourceOperation("software.mac", "MacSoftware"), macSoftware),
		plugin.Register(ops.registry, resourceOperation("software.windows", "WindowsSoftware"), windowsSoftware),
	} {
		if err != nil {
			return err
		}
	}
	ops.identity["build.mac.pkg"] = macpkg.Version
	ops.identity["software.mac"] = macsoftware.Version
	ops.identity["software.windows"] = "windowssoftware/1"
	return nil
}

func resourceOperation(name, kind string) plugin.Operation {
	return plugin.Operation{Name: name, Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "stemma/v1alpha1", Kind: kind}, SideEffects: "workspace", Methods: []string{"discover", "run"}}
}

func buildMacPkg(ctx context.Context, request plugin.ResourceRequest[json.RawMessage]) (plugin.ResourceResult, error) {
	if request.Method == "discover" {
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(request.Config, &spec); err != nil {
			return plugin.ResourceResult{}, err
		}
		var declarations map[string]plugin.Input
		if data, ok := spec["inputs"]; ok {
			if err := json.Unmarshal(data, &declarations); err != nil {
				return plugin.ResourceResult{}, err
			}
		}
		delete(spec, "inputs")
		config, err := json.Marshal(spec)
		return plugin.ResourceResult{Inputs: declarations, Config: config}, err
	}
	artifact, err := macpkg.Prepare(ctx, request)
	return plugin.ResourceResult{Artifacts: map[string]plugin.Artifact{"installer": artifact}}, err
}
func macSoftware(ctx context.Context, request plugin.ResourceRequest[macsoftware.Spec]) (plugin.ResourceResult, error) {
	spec := request.Config
	input := request
	if request.Method == "discover" {
		if spec.Source == nil && (spec.Application != nil || spec.PackagePath != "" || spec.Signature != nil) {
			return plugin.ResourceResult{}, errors.New("application selection, package selection and signature verification require a source")
		}

		declarations := map[string]plugin.Input{}
		if spec.Source != nil {
			declarations["source"] = *spec.Source
		}
		config, err := json.Marshal(spec.Preparation())
		return plugin.ResourceResult{Inputs: declarations, Config: config, Destinations: spec.Destinations, Icon: spec.Icon, MinimumOS: spec.MinimumOS, Subjects: spec.Subjects}, err
	}
	artifacts, err := macsoftware.Prepare(ctx, spec, macsoftware.Request{Input: input.Inputs["source"], Workspace: input.Workspace, Timestamp: input.Timestamp, DeriveSignature: input.Derive == "signature"})
	if installer, ok := artifacts["installer"]; err == nil && ok {
		installer.Filename = artifactname.Filename(input.Identity.Name, installer.Version, installer.SHA256, installer.Format)
		artifacts["installer"] = installer
	}
	return plugin.ResourceResult{Artifacts: artifacts}, err
}
func windowsSoftware(ctx context.Context, request plugin.ResourceRequest[windowssoftware.Spec]) (plugin.ResourceResult, error) {
	spec := request.Config
	input := request
	if request.Method == "discover" {
		declarations := map[string]plugin.Input{"source": spec.Source}
		if spec.Content != nil {
			for name, value := range spec.Content.Files {
				declarations["file:"+name] = value
			}
		}
		destinations, iconName, subjects := spec.Destinations, spec.Icon, spec.Subjects
		spec.Source, spec.Destinations, spec.Icon, spec.Subjects = plugin.Input{}, nil, "", nil
		if spec.Content != nil {
			spec.Content.Files = nil
		}
		config, err := json.Marshal(spec)
		return plugin.ResourceResult{Inputs: declarations, Config: config, Destinations: destinations, Icon: iconName, Subjects: subjects}, err
	}
	if spec.Content != nil {
		spec.Content.Files = map[string]plugin.Input{}
		for name := range input.Inputs {
			if filename, ok := strings.CutPrefix(name, "file:"); ok {
				spec.Content.Files[filename] = plugin.Input{}
			}
		}
	}
	artifacts, err := windowssoftware.Prepare(ctx, spec, input.Inputs, input.Workspace, input.Derive == "signature")
	return plugin.ResourceResult{Artifacts: artifacts}, err
}
