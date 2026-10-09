package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/plugin"
)

// reconcile reconciles one publication once, after the selected publications
// it requires and the preparation of its resource. A failure belongs to the
// resource; the error is one that stops the run.
func (e *execution) reconcile(ctx context.Context, ref destinationRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.reconciled[ref] {
		return nil
	}
	plan := e.destinations[ref]
	for _, dependency := range plan.requires {
		if err := e.reconcile(ctx, dependency); err != nil {
			return err
		}
	}
	if err := e.prepare(ctx, ref.Resource); err != nil {
		return err
	}
	prepared := e.prepared[ref.Resource]
	item := &e.report.Resources[prepared.report]
	e.reconciled[ref] = true
	if !prepared.ready {
		e.failed[ref] = true
		return nil
	}
	var failure error
	if e.publishing() {
		for _, dependency := range plan.requires {
			if e.failed[dependency] {
				failure = fmt.Errorf("required publication %s/%s failed", dependency.Resource, dependency.Destination)
				break
			}
		}
	}
	before := len(item.Destinations)
	// Preparation checks metadata against the prepared artifact, except
	// metadata that reads env values, which only publication evaluates.
	if failure == nil && (e.publishing() || !plan.environment) {
		operation, err := e.session.ops.operation(e.session.project.Destinations[ref.Destination].Operation)
		if err != nil {
			return err
		}
		peers, err := resolvePeerMetadata(ctx, e.plans, ref.Destination, plan.peers, e.prepared, operation.MetadataSchema)
		failure = err
		if failure == nil {
			failure = e.publishTo(ctx, ref, prepared, peers, item)
		}
	}
	if failure != nil {
		e.failed[ref] = true
		if len(item.Destinations) == before {
			item.Destinations = append(item.Destinations, DestinationReport{Name: ref.Destination, Error: failure.Error()})
		}
		if item.Error == "" {
			item.Error = failure.Error()
		} else {
			item.Error += "\n" + failure.Error()
		}
		e.fail(ctx, ResourceError{Resource: ref.Resource, Err: fmt.Errorf("%s: %w", ref.Destination, failure)})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if failure != nil {
		plugin.Logger(ctx).DebugContext(ctx, "Destination failed", "resource", item.Kind+"/"+item.Name, "destination", ref.Destination, "error", failure)
	}
	e.pending[ref.Resource]--
	if e.pending[ref.Resource] == 0 {
		return e.complete(ctx, item)
	}
	return nil
}

func (e *execution) publishTo(ctx context.Context, ref destinationRef, resource preparedResource, peers map[string]json.RawMessage, item *ResourceReport) (runErr error) {
	s, software, destination := e.session, e.plans[ref.Resource], ref.Destination
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", software.Resource.Kind+"/"+software.Resource.Metadata.Name, "destination", destination))
	done := plugin.Stage(ctx, "Validating destination")
	defer func() { done(runErr) }()
	prepared, present := resource.outputs["installer"]
	if reference, ok := software.Destinations[destination]["installer"].(string); ok {
		prepared, present = resource.outputs[reference]
		if !present {
			return fmt.Errorf("references missing output %s", reference)
		}
	}
	d := s.project.Destinations[destination]
	metadata := destinationMetadata(software.Destinations[destination])
	operation, err := s.ops.operation(d.Operation)
	if err != nil {
		return err
	}
	if operation.Content != nil {
		if err := operation.Content.Accepts(prepared.artifact()); err != nil {
			return err
		}
	}
	roots, err := expression.Roots(metadata)
	if err != nil {
		return err
	}
	if present && !prepared.SuppliedFacts && (operation.RequiresInspection || slices.Contains(roots, "facts")) {
		prepared.Facts, err = inspection.Read(ctx, prepared.Path)
		if err != nil {
			return fmt.Errorf("required inspection: %w", err)
		}
	}
	effective, origins, err := resolveMetadata(software.ResourceResult, metadata, prepared.Facts, prepared.Evidence)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	work := filepath.Join(resource.work, "destinations", destination)
	if present {
		prepared, err = materialize(ctx, s.store, prepared, work)
		if err != nil {
			return err
		}
	}
	request, err := e.request(ref, prepared, effective, peers)
	if err != nil {
		return err
	}
	references := map[string]string{}
	for output := range resource.outputs {
		if output != "installer" {
			references[output] = output
		}
	}
	maps.Copy(references, destinationReferences(software.Destinations[destination]))
	for input, reference := range references {
		artifact, exists := resource.outputs[reference]
		if !exists {
			return fmt.Errorf("missing input %s output %s", input, reference)
		}
		artifact, err = materialize(ctx, s.store, artifact, filepath.Join(work, "inputs", input))
		if err != nil {
			return err
		}
		request.Inputs[input] = artifact.artifact()
	}
	if software.Icon != "" && e.publishing() {
		artifact, err := iconInput(s.root, software.Icon, filepath.Join(work, "inputs", "icon"))
		if err != nil {
			return err
		}
		request.Inputs["icon"] = artifact
	}
	if err := s.ops.call(ctx, d.Operation, "validate", request, nil); err != nil {
		return err
	}
	if err := verifyLeases(ctx, request); err != nil {
		return err
	}
	if !e.publishing() {
		return nil
	}
	done(nil)
	report := DestinationReport{Name: destination, Origins: origins, Artifact: prepared.Filename, Version: prepared.Version}
	done = plugin.Stage(ctx, "Planning destination")
	request.Method = "plan"
	request.Config = e.connections[destination]
	var response plugin.ReconcileResponse
	err = s.ops.call(ctx, d.Operation, "plan", request, &response)
	report.Changes = response.Changes
	report.Origins = mergeOrigins(report.Origins, response.Origins)
	err = errors.Join(err, verifyLeases(ctx, request))
	done(err, plugin.Detail(changeCount(len(response.Changes))))
	if err == nil && e.opts.Method == "apply" {
		report, err = e.deliver(ctx, d.Operation, request, report)
	}
	if err != nil {
		report.Error = err.Error()
	}
	item.Destinations = append(item.Destinations, report)
	if err != nil {
		return err
	}
	return nil
}

func (e *execution) request(ref destinationRef, prepared Prepared, metadata map[string]any, peers map[string]json.RawMessage) (plugin.ReconcileRequest[json.RawMessage], error) {
	plan := e.plans[ref.Resource]
	data, err := json.Marshal(metadata)
	if err != nil {
		return plugin.ReconcileRequest[json.RawMessage]{}, err
	}
	minimum, err := minimumOS(prepared.artifact(), plan.MinimumOS)
	if err != nil {
		return plugin.ReconcileRequest[json.RawMessage]{}, err
	}
	return plugin.ReconcileRequest[json.RawMessage]{
		Method: "validate", Identity: plugin.Identity{Project: e.session.project.Project, Resource: plan.Resource.Reference(), Destination: ref.Destination},
		Metadata: data, Artifact: prepared.artifact(), Inputs: map[string]plugin.Artifact{}, MinimumOS: minimum, Prepared: true, Root: e.session.root, Peers: peers,
	}, nil
}

func (e *execution) deliver(ctx context.Context, operation string, request plugin.ReconcileRequest[json.RawMessage], report DestinationReport) (_ DestinationReport, runErr error) {
	done := plugin.Stage(ctx, "Applying destination")
	defer func() { done(runErr) }()
	if err := verifyLeases(ctx, request); err != nil {
		return report, err
	}
	request.Method = "apply"
	var response plugin.ReconcileResponse
	err := e.session.ops.call(ctx, operation, "apply", request, &response)
	err = errors.Join(err, verifyLeases(ctx, request))
	report.Changes = response.Changes
	report.Origins = mergeOrigins(report.Origins, response.Origins)
	report.Applied = err == nil
	done(err, plugin.Detail(changeCount(len(report.Changes))))
	return report, err
}

// connectionSettings evaluates a destination's settings for publication and
// checks them against its operation's schema.
func connectionSettings(ops *operations, name string, destination config.Destination) (json.RawMessage, error) {
	settings, err := destination.ResolvedConfig()
	if err == nil {
		err = ops.configuration(destination.Operation, settings, true)
	}
	if err != nil {
		return nil, fmt.Errorf("destination %s: %w", name, err)
	}
	return json.Marshal(settings)
}

func changeCount(count int) string {
	switch count {
	case 0:
		return "no changes"
	case 1:
		return "1 change"
	}
	return strconv.Itoa(count) + " changes"
}

// verifyLeases checks that an operation left the artifacts it was lent as
// they were.
func verifyLeases(ctx context.Context, request plugin.ReconcileRequest[json.RawMessage]) error {
	var artifacts []plugin.Artifact
	if request.Artifact.Path != "" {
		artifacts = append(artifacts, request.Artifact)
	}
	for _, input := range request.Inputs {
		artifacts = append(artifacts, input)
	}
	for _, artifact := range artifacts {
		ref, err := digestPath(ctx, artifact.Path, artifact.Tree)
		if err != nil {
			return err
		}
		info, err := os.Stat(artifact.Path)
		if err != nil {
			return err
		}
		if ref.SHA256 != artifact.SHA256 || ref.Size != artifact.Size || uint32(info.Mode().Perm()) != artifact.Mode {
			return errors.New("operation modified an immutable leased artifact")
		}
	}
	return nil
}

func destinationReferences(metadata map[string]any) map[string]string {
	result := map[string]string{}
	inputs, _ := metadata["inputs"].(map[string]any)
	for name, value := range inputs {
		if reference, ok := value.(string); ok {
			result[name] = reference
		}
	}
	return result
}
