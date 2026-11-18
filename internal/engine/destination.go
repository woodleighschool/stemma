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
	"strings"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/expression"
	"github.com/woodleighschool/stemma/plugin"
)

// A prepared publication waits on its first unfinished prerequisite. Finishing
// that prerequisite wakes its waiters, even when the prerequisite failed.
func (e *execution) reconcileReady(ctx context.Context, ref destinationRef, waiting map[destinationRef][]destinationRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared := e.prepared[ref.Resource]
	if prepared.ready {
		for _, dependency := range e.destinations[ref].requires {
			if _, done := e.available[dependency]; !done {
				waiting[dependency] = append(waiting[dependency], ref)
				return nil
			}
		}
		if err := e.reconcile(ctx, ref, prepared); err != nil {
			return err
		}
	} else {
		e.available[ref] = false
	}
	for _, dependent := range waiting[ref] {
		if err := e.reconcileReady(ctx, dependent, waiting); err != nil {
			return err
		}
	}
	delete(waiting, ref)
	return nil
}

func (e *execution) reconcile(ctx context.Context, ref destinationRef, prepared preparedResource) error {
	plan := e.destinations[ref]
	item := &e.report.Resources[prepared.report]
	publication := item.addPublication(ref.Destination)
	if e.publishing() {
		for _, dependency := range plan.requires {
			if !e.available[dependency] {
				publication.BlockedBy = append(publication.BlockedBy, dependency.Resource)
			}
		}
	}
	switch {
	case len(publication.BlockedBy) > 0:
		publication.Status = StatusBlocked
		plugin.Logger(ctx).InfoContext(ctx, "Destination blocked", "resource", item.Kind+"/"+item.Name, "destination", ref.Destination, "blocked_by", publication.BlockedBy)
	case !e.publishing() && plan.environment:
		publication.Deferred = "its metadata reads environment values, which plan and apply evaluate"
	default:
		if err := e.publishTo(ctx, ref, prepared, publication); err != nil {
			publication.Status, publication.Error = StatusFailed, err.Error()
			if item.Error != "" {
				item.Error += "\n"
			}
			item.Error += err.Error()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.fail(ctx, ResourceError{Resource: ref.Resource, Err: fmt.Errorf("%s: %w", ref.Destination, err)})
			plugin.Logger(ctx).InfoContext(ctx, "Destination failed", "resource", item.Kind+"/"+item.Name, "destination", ref.Destination, "error", err)
		}
	}
	e.available[ref] = !publication.Status.unsuccessful()
	e.pending[ref.Resource]--
	if e.pending[ref.Resource] == 0 {
		return e.complete(ctx, item)
	}
	return nil
}

func (e *execution) publishTo(ctx context.Context, ref destinationRef, resource preparedResource, publication *Publication) (runErr error) {
	s, software, destination := e.session, e.plans[ref.Resource], ref.Destination
	phase, stage := "Check", "Checking destination configuration"
	switch e.opts.Method {
	case "plan":
		phase, stage = "Plan", "Planning destination"
	case "apply":
		phase, stage = "Publish", "Publishing to destination"
	}
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("resource", software.Resource.Kind+"/"+software.Resource.Metadata.Name, "destination", destination, "phase", phase))
	done := plugin.Stage(ctx, stage)
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
	peers, err := resolvePeerMetadata(ctx, s.store, e.plans, destination, e.destinations[ref].peers, e.prepared, operation.MetadataSchema)
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
		prepared.Facts, err = inspectPrepared(ctx, s.store, prepared)
		if err != nil {
			return fmt.Errorf("required inspection: %w", err)
		}
	}
	effective, origins, err := resolveMetadata(software.ResourceResult, metadata, prepared.Facts, prepared.Evidence)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	work := filepath.Join(resource.work, "destinations", destination)
	prepared.Path = ""
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
		artifact.Path = ""
		request.Inputs[input] = artifact.artifact()
	}
	if software.Icon != "" && e.publishing() {
		artifact, err := iconInput(ctx, s.store, s.root, software.Icon)
		if err != nil {
			return err
		}
		request.Inputs["icon"] = artifact.artifact()
	}
	if e.publishing() {
		request.Method = e.opts.Method
		request.Config = e.connections[destination]
	}
	response, err := e.callDestination(ctx, d.Operation, request, work)
	publication.Metadata, publication.Origins = response.Metadata, mergeOrigins(origins, response.Origins)
	if err == nil && len(response.Metadata) == 0 {
		publication.Deferred = "the " + d.Operation + " destination does not report the values it derives"
	}
	if !e.publishing() {
		return err
	}
	publication.Version, publication.Previous = response.Version, response.Previous
	publication.Uploads, publication.Fields, publication.Removed = response.Uploads, response.Fields, response.Removed
	publication.Repairs = response.Repairs
	publication.Status = StatusUnchanged
	if response.Changed() {
		publication.Status = StatusChanged
	}
	return err
}

// Destination order in a report is independent of when its prerequisites finish.
func (r *ResourceReport) addPublication(destination string) *Publication {
	i, _ := slices.BinarySearchFunc(r.Publications, destination, func(p Publication, name string) int {
		return strings.Compare(p.Destination, name)
	})
	r.Publications = slices.Insert(r.Publications, i, Publication{Destination: destination})
	return &r.Publications[i]
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

// callDestination keeps byte leases private to one method. Apply holds the icon
// from the start: a destination's own writes can displace published artwork
// after its one chance to ask. A consumer requests all other content before
// writes; the retry must finish or fail clearly.
func (e *execution) callDestination(ctx context.Context, operation string, request plugin.ReconcileRequest[json.RawMessage], work string) (plugin.ReconcileResponse, error) {
	var response plugin.ReconcileResponse
	work = filepath.Join(work, request.Method)
	defer func() { cleanupError(ctx, os.RemoveAll(work)) }()
	lease := func(artifact plugin.Artifact, name, directory string) (plugin.Artifact, error) {
		if artifact.SHA256 == "" || artifact.Path != "" {
			return artifact, fmt.Errorf("destination requested unavailable content %q", name)
		}
		prepared := Prepared{Payload: cas.Ref{SHA256: artifact.SHA256, Size: artifact.Size}, Filename: artifact.Filename, Mode: artifact.Mode, Tree: artifact.Tree}
		leased, err := e.session.ops.lease(ctx, e.session.store, operation, prepared, filepath.Join(work, directory))
		artifact.Path = leased.Path
		return artifact, err
	}
	request.Inputs = maps.Clone(request.Inputs)
	var err error
	if icon, declared := request.Inputs["icon"]; declared && request.Method == "apply" {
		request.Inputs["icon"], err = lease(icon, "icon", "input-"+config.Fingerprint("icon"))
		if err != nil {
			return response, err
		}
	}
	if err = e.session.ops.call(ctx, operation, request.Method, request, &response); err != nil || response.Content == nil {
		return response, err
	}
	needed := response.Content
	if !needed.Artifact && len(needed.Inputs) == 0 {
		return response, errors.New("destination requested no content")
	}
	if needed.Artifact {
		request.Artifact, err = lease(request.Artifact, "artifact", "artifact")
		if err != nil {
			return response, err
		}
	}
	for _, name := range needed.Inputs {
		artifact, ok := request.Inputs[name]
		if !ok {
			return response, fmt.Errorf("destination requested unknown input %q", name)
		}
		request.Inputs[name], err = lease(artifact, name, "input-"+config.Fingerprint(name))
		if err != nil {
			return response, err
		}
	}
	response = plugin.ReconcileResponse{}
	err = e.session.ops.call(ctx, operation, request.Method, request, &response)
	if err == nil && response.Content != nil {
		err = errors.New("destination requested content again after materialization; retry the command")
	}
	return response, err
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
