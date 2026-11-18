// Package jamf reconciles packages, install and patch policies, and retention
// with Jamf Pro. It keeps no state between runs: a marker in each package's
// notes identifies the packages that belong to a software, and policies are
// found by name.
package jamf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

// Config is the Jamf Pro server and the API client that publishes to it.
type Config struct {
	URL          string `json:"url" jsonschema:"minLength=1" jsonschema_description:"Jamf Pro server origin, such as https://school.jamfcloud.com. HTTPS is required except loopback test servers. Paths, embedded credentials, queries and fragments are rejected."`
	ClientID     string `json:"client_id" jsonschema:"minLength=1" jsonschema_description:"Jamf API client ID. Use an env expression to supply it from the environment."`
	ClientSecret string `json:"client_secret" jsonschema:"minLength=1,writeOnly=true" jsonschema_description:"Jamf API client secret. Use an env expression to supply it from the environment."`
}

// Handle validates, plans or applies one software's publication. It converges
// the package holding the artifact's file name, then the install policies,
// patch deployment and retention; plan reports the same publication without
// writing.
func Handle(ctx context.Context, request plugin.ReconcileRequest[Config]) (response plugin.ReconcileResponse, err error) {
	config := request.Config
	// Validation also runs before connection settings are available.
	if request.Method != "validate" {
		if err := config.Validate(); err != nil {
			return response, err
		}
	}
	config.URL = strings.TrimRight(config.URL, "/")
	metadata, response, err := deriveMetadata(request)
	if err != nil {
		return response, err
	}
	preview := response
	defer func() {
		response.Metadata, response.Origins = preview.Metadata, preview.Origins
	}()
	if request.Method == "validate" {
		if request.Artifact.SHA256 != "" {
			_, err = describePayload(request.Identity, request.Artifact)
		}
		return response, err
	}
	if request.Method != "plan" && request.Method != "apply" {
		return response, fmt.Errorf("unsupported Jamf method %q", request.Method)
	}
	if request.Artifact.Path == "" {
		return plugin.ReconcileResponse{Content: &plugin.ContentRequest{Artifact: true}}, nil
	}
	content, err := inspectPayload(ctx, request.Identity, request.Artifact)
	if err != nil {
		return response, err
	}
	icon, err := newSelfServiceIcon(request.Inputs["icon"])
	if err != nil {
		return response, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	c, err := newClient(ctx, config)
	if err != nil {
		return response, err
	}
	// Names resolve before anything is written, so a missing or ambiguous one
	// changes nothing.
	packageCategory, err := c.category(ctx, metadata.category)
	if err != nil {
		return response, err
	}
	if packageCategory != nil {
		metadata.fields["categoryId"] = raw(packageCategory.id)
	}
	for _, policy := range metadata.policies {
		if err := c.resolvePolicy(ctx, policy); err != nil {
			return response, err
		}
	}
	if metadata.patch != nil {
		if err := c.resolvePatch(ctx, metadata.patch); err != nil {
			return response, err
		}
	}
	identity := request.Identity.Digest()
	family, err := c.family(ctx, identity)
	if err != nil {
		return response, err
	}
	current, err := c.locate(ctx, family, metadata.packageID, identity, content.filename)
	if err != nil {
		return response, err
	}
	pub := publication{filename: content.filename, version: request.Artifact.Version, icon: icon}
	response.Version = content.filename
	// Jamf identifies a package by filename; its marker identifies the family.
	// Compare metadata with the current package, or the newest family member.
	replaced := current
	if current != nil {
		pub.packageID = stringField(current.Fields, "id")
		response.Previous = stringField(current.Fields, "fileName")
	} else if latest, ok := newest(family); ok {
		response.Previous = latest.FileName
		if replaced, err = c.get(ctx, latest.id()); err != nil {
			return response, err
		}
	}
	if response.Uploads, response.Fields, err = c.planPackage(ctx, current, replaced, metadata, content); err != nil {
		return plugin.ReconcileResponse{}, err
	}
	packageFields, packageUploads := response.Fields, response.Uploads
	for _, policy := range metadata.policies {
		if err := c.planPolicy(ctx, policy, pub, &response); err != nil {
			return plugin.ReconcileResponse{}, err
		}
	}
	if metadata.patch != nil {
		if err := c.planPatch(ctx, metadata.patch, pub, &response); err != nil {
			return plugin.ReconcileResponse{}, err
		}
	}
	iconUploads := icon.upload()
	response.Uploads = append(response.Uploads, iconUploads...)
	// The current record is never retired, so creating it below leaves this
	// selection as planned.
	var retiring []summary
	if metadata.retention.Keep > 0 {
		retiring = retired(family, pub.packageID, metadata.retention.Keep)
	}
	if request.Method == "plan" {
		err = c.prune(ctx, retiring, metadata.patch, metadata.policies, false, &response)
		return response, err
	}
	response = plugin.ReconcileResponse{Metadata: response.Metadata, Origins: response.Origins, Version: response.Previous, Previous: response.Previous}
	defer func() {
		if icon != nil && icon.uploaded {
			response.Uploads = append(response.Uploads, plugin.Upload{Name: plugin.UploadIcon, Filename: icon.filename(), After: icon.artifact.SHA256})
		}
	}()
	pub.packageID, err = c.publishPackage(ctx, current, metadata.fields, identity, request.Artifact.Path, content, &response)
	for i := range response.Uploads {
		if response.Uploads[i].Name == plugin.UploadInstaller && len(packageUploads) > 0 {
			response.Uploads[i].Before = packageUploads[0].Before
		}
	}
	if err != nil {
		return response, err
	}
	response.Fields = append(response.Fields, packageFields...)
	for _, policy := range metadata.policies {
		if err := c.applyPolicy(ctx, policy, pub, &response); err != nil {
			return response, err
		}
	}
	if metadata.patch != nil {
		if err := c.applyPatch(ctx, metadata.patch, pub, &response); err != nil {
			return response, err
		}
	}
	err = c.prune(ctx, retiring, metadata.patch, metadata.policies, true, &response)
	return response, err
}

// Validate checks the server origin and credentials.
func (config Config) Validate() error {
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return errors.New("jamf url must be a server origin without credentials, path, query or fragment")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return errors.New("jamf url must use HTTPS (HTTP is allowed only for loopback test servers)")
	}
	if config.ClientID == "" || config.ClientSecret == "" {
		return errors.New("jamf client_id and client_secret are required")
	}

	return nil
}
