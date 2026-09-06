// Package intune reconciles native Windows and macOS Microsoft Graph app contracts.
// Upload acceptance and endpoint installation require live tenant verification.
package intune

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	abs "github.com/microsoft/kiota-abstractions-go"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/intunecontent"
	"github.com/woodleighschool/stemma/plugin"
)

const appsPath = "/deviceAppManagement/mobileApps"

type binding struct {
	Identity        string         `json:"identity"`
	AppID           string         `json:"app_id,omitempty"`
	PayloadSHA256   string         `json:"payload_sha256,omitempty"`
	EnvelopeSHA256  string         `json:"envelope_sha256,omitempty"`
	ContentVersion  string         `json:"content_version,omitempty"`
	UncertainCreate bool           `json:"uncertain_create,omitempty"`
	Pending         *pendingUpload `json:"pending,omitempty"`
}

type pendingUpload struct {
	PayloadSHA256  string                       `json:"payload_sha256"`
	EnvelopeSHA256 string                       `json:"envelope_sha256,omitempty"`
	VersionID      string                       `json:"version_id,omitempty"`
	FileID         string                       `json:"file_id,omitempty"`
	Name           string                       `json:"name,omitempty"`
	PlaintextSize  int64                        `json:"plaintext_size,omitempty"`
	EncryptedSize  int64                        `json:"encrypted_size,omitempty"`
	EncryptionInfo intunecontent.EncryptionInfo `json:"encryption_info"`
	Stage          string                       `json:"stage"`
}

var markerPattern = regexp.MustCompile(`(?m)^\[stemma:v1 id=([0-9a-f]{64}) payload=([0-9a-f]*) content=([A-Za-z0-9-]*) envelope=([0-9a-f]*)\]$`)

// Handle validates, plans or applies an Intune destination request.
// Callers must persist a returned Binding even when an error reports partial
// progress. Configuration contains credential environment names, never secrets.
func Handle(ctx context.Context, req plugin.Request) (plugin.Response, error) {
	if req.Protocol != 0 && req.Protocol != plugin.ProtocolVersion {
		return plugin.Response{}, errors.New("unsupported plugin protocol")
	}
	cfg, err := parseConfiguration(req.Config)
	if err != nil {
		return plugin.Response{}, err
	}
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		return plugin.Response{}, err
	}
	cfg.AppID = text(desired["app_id"])
	delete(desired, "app_id")
	if req.Method == "validate" {
		return plugin.Response{Protocol: plugin.ProtocolVersion}, nil
	}
	if req.Method != "plan" && req.Method != "apply" {
		return plugin.Response{}, fmt.Errorf("unsupported Intune method %q", req.Method)
	}
	c, err := newClient(cfg)
	if err != nil {
		return plugin.Response{}, err
	}
	return c.handle(ctx, req, cfg, desired)
}

func (c *client) handle(ctx context.Context, req plugin.Request, cfg configuration, desired object) (response plugin.Response, err error) {
	typedClient := *c
	typedClient.appType = text(desired["@odata.type"])
	c = &typedClient
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	response.Protocol = plugin.ProtocolVersion
	id := sha256.Sum256(raw(req.Identity))
	identity := hex.EncodeToString(id[:])
	b := binding{Identity: identity}
	if len(req.Binding) > 0 && string(req.Binding) != "null" {
		if err := json.Unmarshal(req.Binding, &b); err != nil {
			return response, err
		}
		if b.Identity != identity {
			return response, errors.New("intune binding belongs to a different logical identity")
		}
	}
	defer func() { response.Binding = raw(b) }()
	artifact, err := identifyArtifact(req.Artifact, c.appType)
	if err != nil {
		return response, err
	}
	current, err := c.observe(ctx, cfg, &b)
	if err != nil {
		return response, err
	}
	if current == nil && b.UncertainCreate {
		return response, errors.New("prior app creation has uncertain outcome; marker discovery found no app, so creation will not be retried")
	}
	if current != nil {
		if current["@odata.type"] != c.appType {
			return response, errors.New("bound Intune app has a different native subtype")
		}
		if err := recoverMarker(current, &b); err != nil {
			return response, err
		}
		if b.Pending != nil && b.Pending.VersionID != "" && text(current["committedContentVersion"]) == b.Pending.VersionID {
			b.PayloadSHA256 = b.Pending.PayloadSHA256
			b.EnvelopeSHA256 = b.Pending.EnvelopeSHA256
			b.ContentVersion = b.Pending.VersionID
			b.Pending = nil
		}
	}
	contentChanged := current == nil || b.PayloadSHA256 != artifact.identity || b.ContentVersion == "" || b.ContentVersion != text(current["committedContentVersion"])
	if b.Pending != nil && b.Pending.PayloadSHA256 != artifact.identity {
		return response, errors.New("unfinished Intune upload belongs to different content; reconcile it before changing the source")
	}
	if current == nil {
		if err := validateCreation(desired); err != nil {
			return response, err
		}
		response.Changes = append(response.Changes, plugin.Change{Kind: "destination", Action: "create", Field: "app", After: raw(desired)})
	}
	if contentChanged {
		response.Changes = append(response.Changes, plugin.Change{Kind: "content", Field: "payload_sha256", Action: "upload", Before: raw(b.PayloadSHA256), After: raw(artifact.identity)})
	}
	if c.appType == win32Type {
		desired["setupFilePath"] = artifact.setup
	}
	_, changes := metadataPatch(current, desired, b)
	if current != nil {
		response.Changes = append(response.Changes, changes...)
	}
	var assignments []any
	assignmentChanged := false
	if value, owned := desired["assignments"]; owned {
		var existing []any
		if current != nil {
			items, err := c.list(ctx, c.assignments(b.AppID))
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
	if req.Method == "plan" {
		return response, nil
	}
	if len(response.Changes) == 0 && b.Pending == nil {
		return response, nil
	}
	var prepared *preparedArtifact
	if contentChanged && (b.Pending == nil || b.Pending.Stage == "file" || b.Pending.Stage == "version" || b.Pending.Stage == "file-request") {
		prepared, err = prepareArtifact(ctx, req.Artifact, artifact)
		if err != nil {
			return response, err
		}
		defer prepared.close()
	}
	if current == nil {
		body := mergeOwned(nil, desired)
		delete(body, "assignments")
		body["notes"] = withMarker(text(body["notes"]), b)
		body["fileName"] = prepared.name
		if c.appType == win32Type {
			body["setupFilePath"] = prepared.setup
		}
		b.UncertainCreate = true
		if err := c.request(ctx, abs.POST, c.apps(), body, &current); err != nil {
			return response, err
		}
		b.AppID = text(current["id"])
		if b.AppID == "" {
			return response, errors.New("intune creation response omitted app ID; discovery is required")
		}
		b.UncertainCreate = false
	}
	if contentChanged {
		if err := c.upload(ctx, b.AppID, artifact.identity, prepared, &b); err != nil {
			return response, err
		}
	}
	// Re-observe after potentially long uploads so nested unmanaged fields and
	// notes changed by another administrator are preserved by the final PATCH.
	if err := c.request(ctx, abs.GET, c.app(b.AppID), nil, &current); err != nil {
		return response, err
	}
	patch, _ := metadataPatch(current, desired, b)
	if b.Pending != nil && b.Pending.Stage == "committed" {
		patch["committedContentVersion"] = b.Pending.VersionID
		patch["fileName"] = b.Pending.Name
		if c.appType == win32Type {
			patch["setupFilePath"] = artifact.setup
		}
		prospective := b
		prospective.PayloadSHA256 = b.Pending.PayloadSHA256
		prospective.EnvelopeSHA256 = b.Pending.EnvelopeSHA256
		prospective.ContentVersion = b.Pending.VersionID
		patch["notes"] = withMarker(noteText(current, desired), prospective)
	}
	if len(patch) > 0 {
		patch["@odata.type"] = c.appType
		if err := c.request(ctx, abs.PATCH, c.app(b.AppID), patch, nil); err != nil {
			return response, err
		}
	}
	if err := c.waitPublished(ctx, b.AppID, b, &current); err != nil {
		return response, err
	}
	if b.Pending != nil {
		if text(current["committedContentVersion"]) != b.Pending.VersionID {
			return response, errors.New("intune did not activate the committed content version")
		}
		b.PayloadSHA256 = b.Pending.PayloadSHA256
		b.EnvelopeSHA256 = b.Pending.EnvelopeSHA256
		b.ContentVersion = b.Pending.VersionID
		b.Pending = nil
	}
	if residual, _ := metadataPatch(current, desired, b); len(residual) > 0 {
		return response, errors.New("intune metadata readback differs from requested values")
	}
	if assignmentChanged {
		items, err := c.list(ctx, c.assignments(b.AppID))
		if err != nil {
			return response, err
		}
		existing := make([]any, 0, len(items))
		for _, item := range items {
			existing = append(existing, item)
		}
		assignments, _ = reconcileAssignments(existing, desired["assignments"].([]any))
		if err := c.request(ctx, abs.POST, c.assign(b.AppID), object{"mobileAppAssignments": assignments}, nil); err != nil {
			return response, err
		}
		items, err = c.list(ctx, c.assignments(b.AppID))
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
	return response, nil
}

func (c *client) observe(ctx context.Context, cfg configuration, b *binding) (object, error) {
	if cfg.AppID != "" && b.AppID != "" && cfg.AppID != b.AppID {
		return nil, errors.New("configured app_id conflicts with saved Intune binding")
	}
	if b.AppID == "" {
		b.AppID = cfg.AppID
	}
	if b.AppID != "" {
		var app object
		if err := c.request(ctx, abs.GET, c.app(b.AppID), nil, &app); err != nil {
			return nil, fmt.Errorf("read bound app; Stemma will not recreate it implicitly: %w", err)
		}
		return app, nil
	}
	apps, err := c.list(ctx, c.apps())
	if err != nil {
		return nil, err
	}
	var found object
	for _, app := range apps {
		for _, match := range markerPattern.FindAllStringSubmatch(text(app["notes"]), -1) {
			if match[1] != b.Identity {
				continue
			}
			if found != nil {
				return nil, errors.New("multiple Intune apps carry this Stemma identity")
			}
			found = app
		}
	}
	if found != nil {
		b.AppID = text(found["id"])
		b.UncertainCreate = false
	}
	return found, nil
}

func recoverMarker(current object, b *binding) error {
	for _, match := range markerPattern.FindAllStringSubmatch(text(current["notes"]), -1) {
		if match[1] != b.Identity {
			return errors.New("adopted app carries another Stemma identity")
		}
		if b.PayloadSHA256 == "" && match[2] != "" && match[3] == text(current["committedContentVersion"]) {
			b.PayloadSHA256 = match[2]
			b.ContentVersion = match[3]
			b.EnvelopeSHA256 = match[4]
		}
	}
	return nil
}

func noteText(current, desired object) string {
	if value, owned := desired["notes"]; owned {
		return text(value)
	}
	return strings.TrimSuffix(markerPattern.ReplaceAllString(text(current["notes"]), ""), "\n")
}

func withMarker(notes string, b binding) string {
	marker := fmt.Sprintf("[stemma:v1 id=%s payload=%s content=%s envelope=%s]", b.Identity, b.PayloadSHA256, b.ContentVersion, b.EnvelopeSHA256)
	if notes == "" {
		return marker
	}
	return notes + "\n" + marker
}

func metadataPatch(current, desired object, b binding) (object, []plugin.Change) {
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
	notes := withMarker(noteText(current, desired), b)
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

func (c *client) waitPublished(ctx context.Context, id string, b binding, current *object) error {
	for range 360 {
		if err := c.request(ctx, abs.GET, c.app(id), nil, current); err != nil {
			return err
		}
		if text((*current)["publishingState"]) == "published" && (b.Pending == nil || text((*current)["committedContentVersion"]) == b.Pending.VersionID) {
			return nil
		}
		if err := c.pause(ctx); err != nil {
			return err
		}
	}
	return errors.New("intune publication did not complete within poll limit")
}
