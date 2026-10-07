package jamf

import (
	"context"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	sdkclient "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/client"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/packages"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

const packagePath = constants.EndpointJamfProPackagesV1

// observed is a package exactly as Jamf returned it, so a whole-object update
// preserves the fields nothing here manages.
type observed struct {
	Fields map[string]json.RawMessage
	ETag   string
}

// summary is the part of a listed package that identity and retention read.
type summary struct {
	ID       uint64 `json:"id,string"`
	FileName string `json:"fileName"`
	Notes    string `json:"notes"`
}

func (s summary) id() string { return strconv.FormatUint(s.ID, 10) }

type payload struct {
	filename string
	sha256   string
	sha3512  string
	size     int64
}

var readOnlyFields = []string{"id", "indexed", "cloudTransferStatus", "size"}

// defaults is a new package record. It carries the marker from creation, so an
// interrupted run leaves a record the next one finds.
func defaults(filename, identity string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"fileName": raw(filename), "packageName": raw(filename), "notes": raw(marker(identity)), "categoryId": raw("-1"), "priority": raw(10), "fillUserTemplate": raw(false), "osInstall": raw(false), "rebootRequired": raw(false), "suppressEula": raw(false), "suppressFromDock": raw(false), "suppressRegistration": raw(false), "suppressUpdates": raw(false)}
}

func plan(current *observed, desired map[string]json.RawMessage, content payload, identity string) []plugin.Change {
	desired = markedFields(current, desired, identity)
	var changes []plugin.Change
	if current == nil {
		changes = append(changes, plugin.Change{Kind: "content", Field: "package", Action: "create", After: raw(content.filename)})
		current = &observed{Fields: defaults(content.filename, identity)}
	} else if !contentDigestMatches(current, content) {
		changes = append(changes, plugin.Change{Kind: "content", Field: "sha256", Action: "upload", Before: current.Fields["sha256"], After: raw(content.sha256)})
	}
	for _, key := range slices.Sorted(maps.Keys(desired)) {
		if !equalJSON(current.Fields[key], desired[key]) {
			changes = append(changes, plugin.Change{Kind: "metadata", Field: fieldName(key), Action: "set", Before: current.Fields[key], After: desired[key]})
		}
	}
	return changes
}

func inspectPayload(ctx context.Context, identity plugin.Identity, artifact plugin.Artifact) (payload, error) {
	if identity.Project == "" || identity.Resource.Name == "" || identity.Destination == "" {
		return payload{}, errors.New("jamf requires a complete logical identity")
	}
	// Jamf installs a DMG as a filesystem layout copied onto the startup disk,
	// which an application DMG is not.
	switch extension := strings.ToLower(filepath.Ext(artifact.Filename)); {
	case extension == ".dmg" && identity.Resource.Kind == "MacSoftware":
		return payload{}, fmt.Errorf("jamf publishes PKG artifacts, not %s; set package to publish the application in a PKG", artifact.Filename)
	case extension != ".pkg":
		return payload{}, fmt.Errorf("jamf publishes PKG artifacts, not %s", artifact.Filename)
	}
	f, err := os.Open(artifact.Path)
	if err != nil {
		return payload{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return payload{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return payload{}, errors.New("jamf artifact must be a nonempty regular file")
	}
	h, hash3 := sha256.New(), sha3.New512()
	n, err := io.Copy(io.MultiWriter(h, hash3), fileio.Reader{Context: ctx, Reader: f})
	if err != nil {
		return payload{}, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if digest != artifact.SHA256 || n != artifact.Size {
		return payload{}, errors.New("jamf artifact bytes disagree with the immutable artifact descriptor")
	}
	return payload{filename: artifact.Filename, sha256: digest, sha3512: hex.EncodeToString(hash3.Sum(nil)), size: n}, nil
}

func contentMatches(current *observed, content payload) bool {
	return current != nil && stringField(current.Fields, "fileName") == content.filename && contentDigestMatches(current, content)
}

func contentDigestMatches(current *observed, content payload) bool {
	if current == nil || stringField(current.Fields, "cloudTransferStatus") != "READY" {
		return false
	}
	if digest := stringField(current.Fields, "sha3512"); digest != "" {
		return strings.EqualFold(digest, content.sha3512)
	}
	switch stringField(current.Fields, "hashType") {
	case "SHA3_512", "SHA3-512":
		return strings.EqualFold(stringField(current.Fields, "hashValue"), content.sha3512)
	case "SHA_256", "SHA256", "SHA-256":
		return strings.EqualFold(stringField(current.Fields, "hashValue"), content.sha256)
	}
	return strings.EqualFold(stringField(current.Fields, "sha256"), content.sha256)
}

func (c *client) get(ctx context.Context, id string) (*observed, error) {
	if !validID(id) {
		return nil, errors.New("invalid Jamf package ID")
	}
	// Typed package models cannot retain unknown fields or distinguish null
	// from empty strings when merging a full-object update.
	response, data, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		GetBytes(packagePath + "/" + id)
	if response != nil && response.StatusCode() == http.StatusNotFound {
		return nil, nil
	}
	if err := requestError(ctx, response, err); err != nil {
		return nil, err
	}
	fields, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	if stringField(fields, "id") != id {
		return nil, errors.New("jamf response ID does not match requested package")
	}
	return &observed{Fields: fields, ETag: response.Header().Get("ETag")}, nil
}

// family lists the packages whose notes carry this identity's marker.
func (c *client) family(ctx context.Context, identity string) ([]summary, error) {
	listed, err := c.list(ctx, sdkclient.NewRSQLFilterBuilder().Contains("notes", marker(identity)).Build())
	if err != nil {
		return nil, err
	}
	// The filter finds the marker anywhere in the notes; only a marker line is identity.
	return slices.DeleteFunc(listed, func(pkg summary) bool { return !slices.Contains(identities(pkg.Notes), identity) }), nil
}

func (c *client) list(ctx context.Context, filter string) ([]summary, error) {
	objects, err := c.listObjects(ctx, packagePath, map[string]string{"filter": filter})
	if err != nil {
		return nil, err
	}
	listed := make([]summary, 0, len(objects))
	for _, data := range objects {
		var pkg summary
		if err := json.Unmarshal(data, &pkg); err != nil {
			return nil, errors.New("invalid Jamf package listing")
		}
		listed = append(listed, pkg)
	}
	return listed, nil
}

// locate selects the record this artifact publishes into: the package_id pin,
// or the family member holding the artifact's file name. It returns nil when
// that record has yet to be created.
func (c *client) locate(ctx context.Context, family []summary, adopt, identity, filename string) (*observed, error) {
	id := adopt
	if id == "" {
		for _, pkg := range family {
			if pkg.FileName != filename {
				continue
			}
			if id != "" {
				return nil, fmt.Errorf("multiple Jamf packages match file name %q for this software; set package_id to select one", filename)
			}
			id = pkg.id()
		}
	}
	var current *observed
	if id != "" {
		var err error
		if current, err = c.get(ctx, id); err != nil {
			return nil, err
		}
	}
	if adopt != "" {
		if current == nil {
			return nil, fmt.Errorf("jamf package_id %s does not exist", adopt)
		}
		if slices.ContainsFunc(identities(stringField(current.Fields, "notes")), func(found string) bool { return found != identity }) {
			return nil, fmt.Errorf("jamf package %s carries another Stemma identity", adopt)
		}
	}
	if current != nil && stringField(current.Fields, "fileName") == filename {
		return current, nil
	}
	// File names share one distribution point namespace, so taking a name another
	// record holds would replace that package's content.
	holders, err := c.list(ctx, sdkclient.NewRSQLFilterBuilder().EqualTo("fileName", filename).Build())
	if err != nil {
		return nil, err
	}
	for _, holder := range holders {
		if holder.FileName == filename && holder.id() != id {
			return nil, fmt.Errorf("jamf package %s already uses file name %q; set package_id to %s to adopt it", holder.id(), filename, holder.id())
		}
	}
	return current, nil
}

func (c *client) create(ctx context.Context, identity, filename string) (*observed, error) {
	var body packages.RequestPackage
	if err := json.Unmarshal(raw(defaults(filename, identity)), &body); err != nil {
		return nil, err
	}
	// CreateV1 performs a distribution-point preflight inside the create call,
	// so its failure would not say whether a package was written.
	var result packages.CreateResponse
	response, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetHeader("Content-Type", constants.ApplicationJSON).
		SetBody(&body).
		SetResult(&result).
		Post(packagePath)
	createErr := requestError(ctx, response, err)
	if createErr == nil && !validID(result.ID) {
		createErr = errors.New("jamf create response has no valid package ID")
	}
	if createErr != nil {
		// A lost response can hide a package Jamf did create; its marker finds it.
		family, err := c.family(ctx, identity)
		if err != nil {
			return nil, createErr
		}
		created, err := c.locate(ctx, family, "", identity, filename)
		if err != nil || created == nil {
			return nil, createErr
		}
		return created, nil
	}
	created, err := c.get(ctx, result.ID)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, errors.New("created Jamf package could not be read back")
	}
	return created, nil
}

func differs(fields, desired map[string]json.RawMessage) bool {
	for key, value := range desired {
		if !equalJSON(fields[key], value) {
			return true
		}
	}
	return false
}

// merge writes the desired fields that differ and verifies them by readback.
func (c *client) merge(ctx context.Context, current *observed, desired map[string]json.RawMessage, identity string) (*observed, error) {
	if !differs(current.Fields, markedFields(current, desired, identity)) {
		return current, nil
	}
	id := stringField(current.Fields, "id")
	// The update replaces the whole object, so it merges over the newest read.
	fresh, err := c.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, errors.New("jamf package disappeared before metadata update")
	}
	desired = markedFields(fresh, desired, identity)
	if !differs(fresh.Fields, desired) {
		return fresh, nil
	}
	body := maps.Clone(fresh.Fields)
	for _, key := range readOnlyFields {
		delete(body, key)
	}
	maps.Copy(body, desired)
	response, putErr := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetHeader("Content-Type", constants.ApplicationJSON).
		SetHeader("If-Match", fresh.ETag).
		SetBody(body).
		DisableRetry().
		Put(packagePath + "/" + id)
	putErr = requestError(ctx, response, putErr)
	readback, err := c.get(ctx, id)
	if err != nil || readback == nil {
		return nil, errors.New("jamf metadata update could not be read back")
	}
	for key, value := range desired {
		if !equalJSON(readback.Fields[key], value) {
			if putErr != nil {
				return nil, fmt.Errorf("jamf metadata update was not observed: %w", putErr)
			}
			return nil, fmt.Errorf("jamf metadata %s did not match readback", key)
		}
	}
	return readback, nil
}

func (c *client) upload(ctx context.Context, id, file string, content payload) (err error) {
	done := plugin.Stage(ctx, "Uploading Jamf package", plugin.Detail(content.filename))
	defer func() { done(err) }()
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// UploadV1 names the upload after the local path, which need not be the
	// artifact's file name.
	var lastProgress time.Time
	response, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetMultipartFile("file", content.filename, f, content.size, func(_, _ string, current, total int64) {
			if current == total || time.Since(lastProgress) >= 250*time.Millisecond {
				plugin.Logger(ctx).InfoContext(ctx, "Transfer progress", "progress", true, "current", current, "total", total, "unit", "bytes", "progress_final", current == total)
				lastProgress = time.Now()
			}
		}).
		Post(packagePath + "/" + id + "/upload")
	return requestError(ctx, response, err)
}

func (c *client) awaitContent(ctx context.Context, id string, content payload) (result *observed, err error) {
	done := plugin.Stage(ctx, "Waiting for Jamf processing")
	defer func() { done(err) }()
	for attempt := range 31 {
		current, err := c.get(ctx, id)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, errors.New("jamf package disappeared during upload verification")
		}
		if contentMatches(current, content) {
			return current, nil
		}
		status := stringField(current.Fields, "cloudTransferStatus")
		if status == "FAILED" || status == "ERROR" {
			return nil, fmt.Errorf("jamf package cloud transfer %s", status)
		}
		if attempt == 30 {
			break
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("jamf content is not READY with a matching SHA-256 or SHA3-512 after 60 seconds")
}

// publishPackage converges the package record on the artifact and declared
// metadata, creating it when current is nil, and returns its ID.
func (c *client) publishPackage(ctx context.Context, current *observed, desired map[string]json.RawMessage, identity, file string, content payload) (string, error) {
	var err error
	if current == nil {
		if current, err = c.create(ctx, identity, content.filename); err != nil {
			return "", err
		}
	}
	id := stringField(current.Fields, "id")
	// An adopted package takes the file name and marker before any content, so an
	// interrupted run leaves a record the next one finds.
	claim := map[string]json.RawMessage{"fileName": desired["fileName"]}
	if current, err = c.merge(ctx, current, claim, identity); err != nil {
		return "", err
	}
	if !contentDigestMatches(current, content) {
		if err := c.upload(ctx, id, file, content); err != nil {
			return "", err
		}
		if current, err = c.awaitContent(ctx, id, content); err != nil {
			return "", err
		}
	}
	// Declared metadata follows verified content, so a failed upload never leaves
	// new settings over old bytes.
	if current, err = c.merge(ctx, current, desired, identity); err != nil {
		return "", err
	}
	if !contentMatches(current, content) {
		return "", errors.New("jamf content changed during metadata reconciliation")
	}
	return id, nil
}
