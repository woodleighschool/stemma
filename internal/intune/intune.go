// Package intune reconciles native Windows and macOS Microsoft Graph app contracts.
// Upload acceptance and endpoint installation require live tenant verification.
package intune

import (
	"context"
	"encoding/base64"
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
// Plan and apply connect with the credentials in the request Config; validate
// requests carry none.
func Handle(ctx context.Context, req plugin.ReconcileRequest[Config]) (response plugin.ReconcileResponse, err error) {
	req, response.Origins, err = Derive(req)
	if err != nil {
		return response, err
	}
	desired, err := decodeObject(req.Metadata)
	if err != nil {
		return response, err
	}
	_, err = lifecycleMetadata(desired)
	if err != nil {
		return response, err
	}
	if req.Method == "validate" {
		if req.Artifact.Path != "" {
			_, err := identifyArtifact(ctx, req.Artifact, text(desired["@odata.type"]))
			return response, err
		}
		return response, nil
	}
	if req.Method != "plan" && req.Method != "apply" {
		return plugin.ReconcileResponse{}, fmt.Errorf("unsupported Intune method %q", req.Method)
	}
	if err := req.Config.Validate(); err != nil {
		return response, err
	}
	c, err := newClient(req.Config)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	result, err := c.handle(ctx, req, desired)
	result.Origins = response.Origins
	return result, err
}

func (c *client) handle(ctx context.Context, req plugin.ReconcileRequest[Config], desired object) (response plugin.ReconcileResponse, err error) {
	lifecycle, err := lifecycleMetadata(desired)
	if err != nil {
		return response, err
	}
	pinned := text(desired["app_id"])
	desired = maps.Clone(desired)
	for _, key := range []string{"app_id", "dependencies", "supersedes"} {
		delete(desired, key)
	}
	typedClient := *c
	typedClient.appType = text(desired["@odata.type"])
	c = &typedClient
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	artifact, err := identifyArtifact(ctx, req.Artifact, c.appType)
	if err != nil {
		return response, err
	}
	tenant := &tenantApps{client: c}
	identity := req.Identity.Digest()
	current, err := c.observe(ctx, tenant, pinned, identity)
	if err != nil {
		return response, err
	}
	published := publication{identity: identity}
	if current != nil {
		if current["@odata.type"] != c.appType {
			return response, errors.New("intune app has a different native subtype")
		}
		if published, err = recoverMarker(current, identity); err != nil {
			return response, err
		}
	}
	if c.appType == lobType && desired["installAsManaged"] == nil && current["installAsManaged"] == true {
		if err := validateLOB(req.Artifact, nil, true); err != nil {
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
		response.Changes = append(response.Changes, plugin.Change{Kind: "destination", Action: "create", Field: "app", After: raw(reported(desired))})
	}
	if contentChanged {
		response.Changes = append(response.Changes, plugin.Change{Kind: "content", Field: "payload_sha256", Action: "upload", Filename: uploadFilename(req.Identity.Resource.Name, req.Artifact, artifact), Before: raw(published.active(current)), After: raw(artifact.identity)})
	}
	if c.appType == win32Type {
		desired["setupFilePath"] = artifact.setup
	}
	_, changes := metadataPatch(current, desired, published)
	if current != nil {
		response.Changes = append(response.Changes, changes...)
	}
	declaredAssignments, assigning := desired["assignments"].([]any)
	assignmentChanged := false
	if assigning {
		change, err := c.planAssignments(ctx, current, declaredAssignments)
		if err != nil {
			return response, err
		}
		if assignmentChanged = change != nil; assignmentChanged {
			response.Changes = append(response.Changes, *change)
		}
	}
	categoryNames, categorizing := desired["categories"].([]any)
	categoryChanged := false
	if categorizing {
		var categoryChanges []plugin.Change
		if categoryChanges, categoryChanged, err = c.planCategories(ctx, current, categoryNames); err != nil {
			return response, err
		}
		response.Changes = append(response.Changes, categoryChanges...)
	}
	relating := lifecycle.Dependencies != nil || lifecycle.Supersedes != nil
	if relating {
		change, err := c.planRelationships(ctx, req, tenant, lifecycle, current)
		if err != nil {
			return response, err
		}
		if change != nil {
			response.Changes = append(response.Changes, *change)
		}
	}
	if req.Method == "plan" {
		return response, nil
	}
	if len(response.Changes) == 0 && current["publishingState"] == "published" {
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
		// Graph refuses notes updates until publication, so the app records its
		// first payload when it is created.
		published.payload = artifact.identity
		if current, err = c.create(ctx, desired, published, prepared); err != nil {
			return response, err
		}
	}
	appID := text(current["id"])
	activated := ""
	if contentChanged {
		pending := publication{identity: identity, payload: artifact.identity}
		if published.payload == pending.payload {
			pending.content = published.content
		}
		if activated, err = c.activate(ctx, current, pending, prepared, artifact.setup); err != nil {
			return response, err
		}
		published = publication{identity: identity, payload: artifact.identity, content: activated}
	}
	// Waiting re-observes the app after potentially long uploads, so nested
	// omitted fields and notes changed by another administrator are preserved.
	if err := c.waitPublished(ctx, appID, activated, &current); err != nil {
		return response, err
	}
	if err := c.applyMetadata(ctx, appID, activated, &current, desired, published); err != nil {
		return response, err
	}
	if _, err := c.reconcileIcon(ctx, req, current, true); err != nil {
		return response, err
	}
	if assignmentChanged {
		if err := c.applyAssignments(ctx, appID, declaredAssignments); err != nil {
			return response, err
		}
	}
	if categoryChanged {
		if err := c.reconcileCategories(ctx, appID, categoryNames); err != nil {
			return response, err
		}
	}
	if relating {
		if err := c.applyRelationships(ctx, req, tenant, lifecycle, appID); err != nil {
			return response, err
		}
	}
	return response, nil
}

func (c *client) create(ctx context.Context, desired object, published publication, prepared *preparedArtifact) (object, error) {
	body := mergeOwned(nil, desired)
	delete(body, "assignments")
	delete(body, "categories")
	body["notes"] = withMarker(text(body["notes"]), published)
	body["fileName"] = prepared.name
	if c.appType == win32Type {
		body["setupFilePath"] = prepared.setup
	}
	var created object
	if err := c.request(ctx, abs.POST, c.apps(), body, &created); err != nil {
		return nil, err
	}
	if text(created["id"]) == "" {
		return nil, errors.New("intune creation response omitted app ID")
	}
	return created, nil
}

func (c *client) activate(ctx context.Context, current object, pending publication, prepared *preparedArtifact, setup string) (string, error) {
	version, err := c.upload(ctx, current, pending, prepared)
	if err != nil {
		return "", err
	}
	// Graph drops every other property of the PATCH that first publishes an
	// app, so activation goes alone and metadata follows publication.
	activation := object{"@odata.type": c.appType, "committedContentVersion": version, "fileName": prepared.name}
	if c.appType == win32Type {
		activation["setupFilePath"] = setup
	}
	if err := c.request(ctx, abs.PATCH, c.app(text(current["id"])), activation, nil); err != nil {
		return "", err
	}
	return version, nil
}

func (c *client) applyMetadata(ctx context.Context, appID, activated string, current *object, desired object, published publication) error {
	if patch, _ := metadataPatch(*current, desired, published); len(patch) > 0 {
		patch["@odata.type"] = c.appType
		if err := c.request(ctx, abs.PATCH, c.app(appID), patch, nil); err != nil {
			return err
		}
		if err := c.waitPublished(ctx, appID, activated, current); err != nil {
			return err
		}
	}
	if _, residual := metadataPatch(*current, desired, published); len(residual) > 0 {
		fields := make([]string, 0, len(residual))
		for _, change := range residual {
			fields = append(fields, change.Field)
		}
		return fmt.Errorf("intune metadata readback differs from requested values: %s", strings.Join(fields, ", "))
	}
	return nil
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
		if key == "@odata.type" || key == "assignments" || key == "categories" || key == "notes" {
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
		if slices.Contains(installScripts, key) {
			action := "set"
			if value == nil {
				action = "clear"
			}
			changes = append(changes, plugin.Change{Kind: "metadata", Field: reportName(key), Action: action})
			continue
		}
		before, after := current[key], value
		if key == "minimumSupportedOperatingSystem" {
			before, after = minimumOSChangeValue(before), minimumOSChangeValue(desired[key])
		}
		changes = append(changes, plugin.Change{Kind: "metadata", Field: reportName(key), Action: "set", Before: raw(before), After: raw(after)})
	}
	notes := withMarker(noteText(current, desired), published)
	if notes != text(current["notes"]) {
		patch["notes"] = notes
		changes = append(changes, plugin.Change{Kind: "metadata", Field: "notes", Action: "set", Before: raw(current["notes"]), After: raw(notes)})
	}
	return patch, changes
}

// installScripts are the Graph properties that hold a PKG app's scripts.
var installScripts = []string{"preInstallScript", "postInstallScript"}

// reported returns the app as a creation reports it. Scripts can embed
// environment values, so the report gives their length instead.
func reported(app object) object {
	app = maps.Clone(app)
	for _, key := range installScripts {
		script, declared := app[key].(object)
		if !declared {
			continue
		}
		content, _ := base64.StdEncoding.DecodeString(text(script["scriptContent"]))
		lines := strings.Count(strings.TrimSuffix(string(content), "\n"), "\n") + 1
		app[key] = fmt.Sprintf("%d lines", lines)
		if lines == 1 {
			app[key] = "1 line"
		}
	}
	return app
}

func minimumOSChangeValue(value any) any {
	if selected := selectedOS(value); selected != "" {
		return strings.ReplaceAll(strings.TrimPrefix(selected, "v"), "_", ".")
	}
	return value
}

// validateCreation checks the fields Graph requires to create an app, named as
// declarations name them.
func validateCreation(m object) error {
	required := []string{"displayName", "description", "publisher"}
	if m["@odata.type"] == win32Type {
		required = append(required, "installCommandLine", "uninstallCommandLine", "minimumSupportedWindowsRelease")
	}
	for _, key := range required {
		if text(m[key]) == "" {
			return fmt.Errorf("creating an Intune app requires %s", reportName(key))
		}
	}
	if m["@odata.type"] != win32Type {
		if selectedOS(m["minimumSupportedOperatingSystem"]) == "" {
			return errors.New("creating a macOS app requires a minimum OS; set minimum_os")
		}
		apps, _ := m["includedApps"].([]any)
		primary := text(m["primaryBundleId"]) != "" && text(m["primaryBundleVersion"]) != ""
		if m["@odata.type"] == lobType {
			apps, _ = m["childApps"].([]any)
			primary = text(m["bundleId"]) != "" && text(m["buildNumber"]) != ""
		}
		if len(apps) == 0 || !primary {
			return errors.New("creating a macOS app requires included_apps")
		}
		return nil
	}
	install, _ := m["installExperience"].(object)
	if text(install["runAsAccount"]) == "" {
		return errors.New("creating a Win32 app requires install_experience.run_as")
	}
	if text(m["allowedArchitectures"]) == "" {
		return errors.New("creating a Win32 app requires architectures")
	}
	rules, _ := m["rules"].([]any)
	if len(rules) == 0 {
		return errors.New("creating a Win32 app requires detection")
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
