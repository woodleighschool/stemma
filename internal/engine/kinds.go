package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/woodleighschool/stemma/internal/macpkg"
	"github.com/woodleighschool/stemma/internal/macsoftware"
	"github.com/woodleighschool/stemma/internal/windowssoftware"
	"github.com/woodleighschool/stemma/plugin"
)

func registerKinds(ops *operations) error {
	for _, item := range []struct {
		name, kind, version string
		spec                any
		handle              plugin.Handler
	}{
		{"build.mac.pkg", "BuildMacPkg", macpkg.Version, macpkg.Spec{}, buildMacPkg},
		{"software.mac", "MacSoftware", macsoftware.Version, macsoftware.Spec{}, macSoftware},
		{"software.windows", "WindowsSoftware", "windowssoftware/1", windowssoftware.Spec{}, windowsSoftware},
	} {
		op := plugin.Operation{Name: item.name, Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "stemma/v1alpha1", Kind: item.kind}, SideEffects: "workspace", Methods: []string{"validate", "run"}, ConfigSchema: operationSchema(item.spec), InputSchema: operationSchema(plugin.ResourceRequest{}), OutputSchema: operationSchema(plugin.ResourceResult{})}
		if err := ops.registry.Register(op, item.handle); err != nil {
			return err
		}
		ops.identity[item.name] = item.version
	}
	return nil
}

func resourceRequest(request plugin.Request, spec any) (plugin.ResourceRequest, error) {
	var input plugin.ResourceRequest
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return input, err
	}
	decoder := json.NewDecoder(bytes.NewReader(input.Config))
	decoder.DisallowUnknownFields()
	return input, decoder.Decode(spec)
}
func resourceResponse(result plugin.ResourceResult, err error) (plugin.Response, error) {
	data, encodeErr := json.Marshal(result)
	return plugin.Response{Output: data}, errors.Join(err, encodeErr)
}
func buildMacPkg(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	var spec macpkg.Spec
	input, err := resourceRequest(request, &spec)
	if err != nil {
		return plugin.Response{}, err
	}
	if request.Method == "validate" {
		if err := spec.Validate(); err != nil {
			return plugin.Response{}, err
		}
		declarations := spec.Inputs
		spec.Inputs = nil
		config, err := json.Marshal(spec)
		return resourceResponse(plugin.ResourceResult{Inputs: declarations, Config: config}, err)
	}
	artifact, err := macpkg.Build(ctx, spec, input.Inputs, input.Workspace, input.Timestamp)
	return resourceResponse(plugin.ResourceResult{Artifacts: map[string]plugin.Artifact{"installer": artifact}}, err)
}
func macSoftware(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	var spec macsoftware.Spec
	input, err := resourceRequest(request, &spec)
	if err != nil {
		return plugin.Response{}, err
	}
	if request.Method == "validate" {
		if spec.Source == nil && (spec.Application != nil || spec.PackagePath != "" || spec.Verification != (macsoftware.Verification{})) {
			return plugin.Response{}, errors.New("application selection, package selection and verification require a source")
		}

		if err := spec.Validate(); err != nil {
			return plugin.Response{}, err
		}
		declarations := map[string]plugin.Input{}
		if spec.Source != nil {
			declarations["source"] = *spec.Source
		}
		config, err := json.Marshal(spec.Preparation())
		return resourceResponse(plugin.ResourceResult{Inputs: declarations, Config: config, Destinations: spec.Destinations}, err)
	}
	artifacts, err := macsoftware.Prepare(ctx, spec, input.Inputs["source"], input.Workspace, input.Timestamp)
	return resourceResponse(plugin.ResourceResult{Artifacts: artifacts}, err)
}
func windowsSoftware(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	var spec windowssoftware.Spec
	input, err := resourceRequest(request, &spec)
	if err != nil {
		return plugin.Response{}, err
	}
	if request.Method == "validate" {
		if err := spec.Validate(); err != nil {
			return plugin.Response{}, err
		}
		declarations := map[string]plugin.Input{"source": spec.Source}
		if spec.Content != nil {
			for name, value := range spec.Content.Files {
				declarations["file:"+name] = value
			}
		}
		destinations := spec.Destinations
		spec.Source = plugin.Input{}
		spec.Destinations = nil
		if spec.Content != nil {
			spec.Content.Files = nil
		}
		config, err := json.Marshal(spec)
		return resourceResponse(plugin.ResourceResult{Inputs: declarations, Config: config, Destinations: destinations}, err)
	}
	if spec.Content != nil {
		spec.Content.Files = map[string]plugin.Input{}
		for name := range input.Inputs {
			if filename, ok := strings.CutPrefix(name, "file:"); ok {
				spec.Content.Files[filename] = plugin.Input{}
			}
		}
	}
	artifacts, err := windowssoftware.Prepare(ctx, spec, input.Inputs, input.Workspace, input.Timestamp)
	return resourceResponse(plugin.ResourceResult{Artifacts: artifacts}, err)
}
