// Package intune reconciles native Windows and macOS Microsoft Graph app contracts.
// Upload acceptance and endpoint installation require live tenant verification.
package intune

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/plugin"
)

const appsPath = "/deviceAppManagement/mobileApps"

// publication is the marker kept in an app's notes. It identifies the app and,
// because Graph exposes no plaintext digest, it is the only record of the
// payload a content version holds.
type publication struct {
	identity string
	payload  string
	content  string
}

var markerPattern = regexp.MustCompile(`(?m)^\[stemma:v1 id=([0-9a-f]{64}) payload=([0-9a-f]*) content=([A-Za-z0-9-]*)\]$`)

// markerIdentity is the marker id of a logical identity. It finds this app and
// the apps of referenced software in the tenant.
func markerIdentity(id plugin.Identity) string {
	sum := sha256.Sum256(raw([]string{id.Project, id.Resource.Key(), id.Destination}))
	return hex.EncodeToString(sum[:])
}

// active returns the payload the app's committed content version holds, or ""
// when the marker does not describe that version.
func (p publication) active(app object) string {
	if p.content == "" || p.content != text(app["committedContentVersion"]) {
		return ""
	}
	return p.payload
}

// tenantApps lists the tenant's apps at most once per invocation. Finding this
// app and the apps of referenced software share the listing.
type tenantApps struct {
	client *client
	apps   []object
	listed bool
}

// find returns the ID of the app whose notes carry the marker of identity, or
// "" when no app does.
func (t *tenantApps) find(ctx context.Context, identity string) (string, error) {
	if !t.listed {
		apps, err := t.client.list(ctx, t.client.apps())
		if err != nil {
			return "", err
		}
		t.apps, t.listed = apps, true
	}
	var found []string
	for _, app := range t.apps {
		carries := func(match []string) bool { return match[1] == identity }
		if slices.ContainsFunc(markerPattern.FindAllStringSubmatch(text(app["notes"]), -1), carries) {
			found = append(found, text(app["id"]))
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("multiple Intune apps carry this Stemma identity: %s", strings.Join(found, ", "))
	}
}

// Handle validates, plans or applies an Intune destination request. It keeps no
// state between invocations: a declared app_id or the marker in an app's notes
// identifies the app, and the tenant supplies its publication state.
// Connection credentials are supplied through the request configuration.
func Handle(ctx context.Context, req plugin.ReconcileRequest) (response plugin.ReconcileResponse, err error) {
	req, response.Origins, err = Derive(req)
	if err != nil {
		return response, err
	}
	cfg, err := parseConfiguration(req.Config)
	if err != nil {
		return response, err
	}
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		return response, err
	}
	_, err = lifecycleMetadata(desired)
	if err != nil {
		return response, err
	}
	if req.Method == "validate" {
		if req.Artifact.Path != "" {
			_, err := identifyArtifact(ctx, req.Artifact, text(desired["@odata.type"]), setupFile(desired))
			return response, err
		}
		return response, nil
	}
	if req.Method != "plan" && req.Method != "apply" {
		return plugin.ReconcileResponse{}, fmt.Errorf("unsupported Intune method %q", req.Method)
	}
	c, err := newClient(cfg)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	result, err := c.handle(ctx, req, desired)
	result.Origins = response.Origins
	return result, err
}

func (c *client) handle(ctx context.Context, req plugin.ReconcileRequest, desired object) (response plugin.ReconcileResponse, err error) {
	lifecycle, err := lifecycleMetadata(desired)
	if err != nil {
		return response, err
	}
	setup := setupFile(desired)
	pinned := text(desired["app_id"])
	desired = maps.Clone(desired)
	for _, key := range []string{"app_id", "retention", "dependencies", "supersedes", "content"} {
		delete(desired, key)
	}
	typedClient := *c
	typedClient.appType = text(desired["@odata.type"])
	c = &typedClient
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	artifact, err := identifyArtifact(ctx, req.Artifact, c.appType, setup)
	if err != nil {
		return response, err
	}
	tenant := &tenantApps{client: c}
	identity := markerIdentity(req.Identity)
	current, err := c.observe(ctx, tenant, pinned, identity)
	if err != nil {
		return response, err
	}
	published := publication{identity: identity}
	appID := text(current["id"])
	if current != nil {
		if current["@odata.type"] != c.appType {
			return response, errors.New("intune app has a different native subtype")
		}
		if published, err = recoverMarker(current, identity); err != nil {
			return response, err
		}
	}
	iconChanges, err := c.reconcileIcon(ctx, req, current, false)
	if err != nil {
		return response, err
	}
	response.Changes = append(response.Changes, iconChanges...)
	contentChanged := published.active(current) != artifact.identity
	if current == nil {
		if err := validateCreation(desired); err != nil {
			return response, err
		}
		response.Changes = append(response.Changes, plugin.Change{Kind: "destination", Action: "create", Field: "app", After: raw(desired)})
	}
	if contentChanged {
		response.Changes = append(response.Changes, plugin.Change{Kind: "content", Field: "payload_sha256", Action: "upload", Before: raw(published.active(current)), After: raw(artifact.identity)})
	}
	if c.appType == win32Type {
		desired["setupFilePath"] = artifact.setup
	}
	_, changes := metadataPatch(current, desired, published)
	if current != nil {
		response.Changes = append(response.Changes, changes...)
	}
	var assignments []any
	assignmentChanged := false
	if value, owned := desired["assignments"]; owned {
		var existing []any
		if current != nil {
			items, err := c.list(ctx, c.assignments(appID))
			if err != nil {
				return response, err
			}
			for _, item := range items {
				existing = append(existing, item)
			}
		}
		assignments, assignmentChanged = reconcileAssignments(existing, value.([]any))
		if assignmentChanged {
			response.Changes = append(response.Changes, plugin.Change{Kind: "assignments", Field: "assignments", Action: "replace", Before: raw(existing), After: raw(assignments)})
		}
	}
	var relationships []object
	var relationshipChanged bool
	if lifecycle.Dependencies != nil || lifecycle.Supersedes != nil {
		wanted, err := c.desiredRelationships(ctx, req, tenant, lifecycle, appID)
		if err != nil {
			return response, err
		}
		var existing []object
		if current != nil {
			existing, err = c.list(ctx, c.relationships(appID))
			if err != nil {
				return response, err
			}
		}
		relationships, relationshipChanged, err = mergeRelationships(existing, wanted, lifecycle)
		if err != nil {
			return response, err
		}
		if relationshipChanged {
			if err := c.checkRelationships(ctx, appID, relationships, existing); err != nil {
				return response, err
			}
			response.Changes = append(response.Changes, plugin.Change{Kind: "relationships", Field: "relationships", Action: "replace", Before: raw(existing), After: raw(relationships)})
		}
	}
	if req.Method == "plan" {
		if lifecycle.Retention == nil || current == nil {
			return response, nil
		}
		// Publishing activates a new version, so every existing version then
		// competes as an earlier publication.
		active := text(current["committedContentVersion"])
		if contentChanged {
			active = ""
		}
		changes, err := c.pruneContent(ctx, appID, active, lifecycle.Retention.Keep, false)
		response.Changes = append(response.Changes, changes...)
		return response, err
	}
	if len(response.Changes) == 0 && lifecycle.Retention == nil && current["publishingState"] == "published" {
		return response, nil
	}
	var prepared *preparedArtifact
	if contentChanged {
		prepared, err = prepareArtifact(ctx, req.Identity.Resource.Name, req.Artifact, artifact)
		if err != nil {
			return response, err
		}
		defer prepared.close()
	}
	if current == nil {
		body := mergeOwned(nil, desired)
		delete(body, "assignments")
		body["notes"] = withMarker(text(body["notes"]), published)
		body["fileName"] = prepared.name
		if c.appType == win32Type {
			body["setupFilePath"] = prepared.setup
		}
		if err := c.request(ctx, abs.POST, c.apps(), body, &current); err != nil {
			return response, err
		}
		appID = text(current["id"])
		if appID == "" {
			return response, errors.New("intune creation response omitted app ID")
		}
	}
	if contentChanged {
		version, err := c.upload(ctx, appID, prepared)
		if err != nil {
			return response, err
		}
		published = publication{identity: identity, payload: artifact.identity, content: version}
	}
	// Re-observe after potentially long uploads so nested omitted fields and
	// notes changed by another administrator are preserved by the final PATCH.
	if err := c.request(ctx, abs.GET, c.app(appID), nil, &current); err != nil {
		return response, err
	}
	patch, _ := metadataPatch(current, desired, published)
	activated := ""
	if contentChanged {
		// Activation travels with its marker so the notes never describe
		// content other than the committed version.
		activated = published.content
		patch["committedContentVersion"] = activated
		patch["notes"] = withMarker(noteText(current, desired), published)
		patch["fileName"] = prepared.name
		if c.appType == win32Type {
			patch["setupFilePath"] = artifact.setup
		}
	}
	if len(patch) > 0 {
		patch["@odata.type"] = c.appType
		if err := c.request(ctx, abs.PATCH, c.app(appID), patch, nil); err != nil {
			return response, err
		}
	}
	if err := c.waitPublished(ctx, appID, activated, &current); err != nil {
		return response, err
	}
	if residual, _ := metadataPatch(current, desired, published); len(residual) > 0 {
		return response, errors.New("intune metadata readback differs from requested values")
	}
	if _, err := c.reconcileIcon(ctx, req, current, true); err != nil {
		return response, err
	}
	if assignmentChanged {
		items, err := c.list(ctx, c.assignments(appID))
		if err != nil {
			return response, err
		}
		existing := make([]any, 0, len(items))
		for _, item := range items {
			existing = append(existing, item)
		}
		assignments, _ = reconcileAssignments(existing, desired["assignments"].([]any))
		if err := c.request(ctx, abs.POST, c.assign(appID), object{"mobileAppAssignments": assignments}, nil); err != nil {
			return response, err
		}
		items, err = c.list(ctx, c.assignments(appID))
		if err != nil {
			return response, err
		}
		existing = existing[:0]
		for _, item := range items {
			existing = append(existing, item)
		}
		if _, changed := reconcileAssignments(existing, desired["assignments"].([]any)); changed {
			return response, errors.New("intune assignments readback differs from requested targeting")
		}
	}
	if lifecycle.Dependencies != nil || lifecycle.Supersedes != nil {
		// Preserve omitted categories against changes during a long content upload.
		existing, err := c.list(ctx, c.relationships(appID))
		if err != nil {
			return response, err
		}
		wanted, err := c.desiredRelationships(ctx, req, tenant, lifecycle, appID)
		if err != nil {
			return response, err
		}
		relationships, changed, err := mergeRelationships(existing, wanted, lifecycle)
		if err != nil {
			return response, err
		}
		if err := c.checkRelationships(ctx, appID, relationships, existing); err != nil {
			return response, err
		}
		if changed {
			if err := c.request(ctx, abs.POST, c.updateRelationships(appID), object{"relationships": relationships}, nil); err != nil {
				return response, err
			}
		}
		readback, err := c.list(ctx, c.relationships(appID))
		if err != nil {
			return response, err
		}
		expected := slices.Clone(relationships)
		for _, item := range existing {
			if item["targetType"] == "parent" {
				expected = append(expected, item)
			}
		}
		if !sameRelationships(readback, expected) {
			return response, errors.New("intune relationship readback differs from requested references")
		}
	}
	if lifecycle.Retention != nil {
		changes, err := c.pruneContent(ctx, appID, text(current["committedContentVersion"]), lifecycle.Retention.Keep, true)
		response.Changes = append(response.Changes, changes...)
		if err != nil {
			return response, err
		}
	}
	return response, nil
}

// observe reads the app this identity manages: the one a declared app_id pins,
// or else the one whose notes carry the identity marker. It returns nil when no
// app exists yet.
func (c *client) observe(ctx context.Context, tenant *tenantApps, pinned, identity string) (object, error) {
	id := pinned
	if id == "" {
		found, err := tenant.find(ctx, identity)
		if err != nil || found == "" {
			return nil, err
		}
		id = found
	}
	// Collection responses can omit heavyweight properties such as largeIcon.
	var app object
	if err := c.request(ctx, abs.GET, c.app(id), nil, &app); err != nil {
		if pinned != "" && errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("app_id %q does not exist in Intune", pinned)
		}
		return nil, fmt.Errorf("read Intune app: %w", err)
	}
	return app, nil
}

// recoverMarker reads the publication recorded in the app's notes. An app that
// carries another identity's marker belongs to other software.
func recoverMarker(app object, identity string) (publication, error) {
	published := publication{identity: identity}
	for _, match := range markerPattern.FindAllStringSubmatch(text(app["notes"]), -1) {
		if match[1] != identity {
			return published, errors.New("intune app carries another Stemma identity")
		}
		published.payload, published.content = match[2], match[3]
	}
	return published, nil
}

func noteText(current, desired object) string {
	if value, owned := desired["notes"]; owned {
		return text(value)
	}
	return strings.TrimSuffix(markerPattern.ReplaceAllString(text(current["notes"]), ""), "\n")
}

func withMarker(notes string, p publication) string {
	marker := fmt.Sprintf("[stemma:v1 id=%s payload=%s content=%s]", p.identity, p.payload, p.content)
	if notes == "" {
		return marker
	}
	return notes + "\n" + marker
}

func metadataPatch(current, desired object, published publication) (object, []plugin.Change) {
	patch := object{}
	var changes []plugin.Change
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if key == "@odata.type" || key == "assignments" || key == "notes" {
			continue
		}
		value := desired[key]
		if (key == "minimumSupportedOperatingSystem" && selectedOS(current[key]) == selectedOS(value)) || ownedEqual(current[key], value) {
			continue
		}
		if child, ok := value.(object); ok {
			previous, _ := current[key].(object)
			if key == "minimumSupportedOperatingSystem" {
				value = mergeOwned(nil, child)
				for old, enabled := range previous {
					if enabled == true {
						value.(object)[old] = false
					}
				}
				maps.Copy(value.(object), child)
			} else {
				value = mergeOwned(previous, child)
			}
		}
		if key == "rules" || key == "returnCodes" {
			previous, _ := current[key].([]any)
			value = mergeItems(key, previous, value.([]any))
		}
		patch[key] = value
		changes = append(changes, plugin.Change{Kind: "metadata", Field: key, Action: "set", Before: raw(current[key]), After: raw(value)})
	}
	notes := withMarker(noteText(current, desired), published)
	if notes != text(current["notes"]) {
		patch["notes"] = notes
		changes = append(changes, plugin.Change{Kind: "metadata", Field: "notes", Action: "set", Before: raw(current["notes"]), After: raw(notes)})
	}
	return patch, changes
}

func validateCreation(m object) error {
	required := []string{"displayName", "description", "publisher"}
	if m["@odata.type"] == win32Type {
		required = append(required, "installCommandLine", "uninstallCommandLine", "minimumSupportedWindowsRelease")
	} else {
		required = append(required, "primaryBundleId", "primaryBundleVersion")
	}
	for _, key := range required {
		if text(m[key]) == "" {
			return fmt.Errorf("creating an Intune app requires %s", key)
		}
	}
	if m["@odata.type"] != win32Type {
		if err := validateMinimumOS(m["minimumSupportedOperatingSystem"]); err != nil {
			return err
		}
		if err := validateIncludedApps(m["includedApps"]); err != nil {
			return err
		}
		return nil
	}
	install, _ := m["installExperience"].(object)
	if text(install["runAsAccount"]) == "" {
		return errors.New("creating a Win32 app requires installExperience.runAsAccount")
	}
	if text(m["allowedArchitectures"]) == "" {
		return errors.New("creating a Win32 app requires allowedArchitectures")
	}
	rules, _ := m["rules"].([]any)
	if len(rules) == 0 {
		return errors.New("creating a Win32 app requires detection rules")
	}
	return nil
}

// waitPublished polls until the app is published and, when this run activated a
// content version, until Graph reports that version as committed.
func (c *client) waitPublished(ctx context.Context, id, activated string, current *object) error {
	for range 360 {
		if err := c.request(ctx, abs.GET, c.app(id), nil, current); err != nil {
			return err
		}
		if text((*current)["publishingState"]) == "published" && (activated == "" || text((*current)["committedContentVersion"]) == activated) {
			return nil
		}
		if err := c.pause(ctx); err != nil {
			return err
		}
	}
	return errors.New("intune publication did not complete within poll limit")
}
