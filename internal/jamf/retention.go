package jamf

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	titles "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_software_title_configurations"
	"github.com/woodleighschool/stemma/plugin"
)

func decodeRetention(metadata map[string]json.RawMessage) (plugin.Retention, error) {
	var retention plugin.Retention
	if data, ok := metadata["retention"]; ok {
		if err := strictDecode(data, &retention); err != nil {
			return retention, fmt.Errorf("jamf retention: %w", err)
		}
		if err := retention.Validate(); err != nil {
			return retention, err
		}
	}
	return retention, nil
}

type references struct {
	packages map[string]bool
	targets  map[string]bool
	titles   map[string]*titles.ResourcePatchSoftwareTitleConfiguration
}

func (c *client) references(ctx context.Context) (references, error) {
	refs := references{packages: map[string]bool{}, targets: map[string]bool{}, titles: map[string]*titles.ResourcePatchSoftwareTitleConfiguration{}}
	policies, err := c.readXML(ctx, "/JSSResource/policies", "policies")
	if err != nil {
		return refs, err
	}
	count, err := strconv.Atoi(policies.value("size"))
	if err != nil || count < 0 {
		return refs, errors.New("jamf policy enumeration has no valid count")
	}
	seen := map[string]bool{}
	for _, summary := range policies.Children {
		if summary.XMLName.Local != "policy" {
			continue
		}
		id := summary.value("id")
		if !validID(id) || seen[id] {
			return refs, errors.New("jamf policy enumeration contains invalid or duplicate IDs")
		}
		seen[id] = true
		policy, err := c.readXML(ctx, "/JSSResource/policies/id/"+id, "policy")
		if err != nil {
			return refs, err
		}
		if policy.value("general", "id") != id {
			return refs, errors.New("jamf policy response ID does not match request")
		}
		configuration := policy.child("package_configuration")
		if configuration == nil || configuration.child("packages") == nil {
			return refs, errors.New("jamf policy package references were omitted")
		}
		for _, pkg := range configuration.child("packages").Children {
			if pkg.XMLName.Local != "package" {
				continue
			}
			id := pkg.value("id")
			if !validID(id) {
				return refs, errors.New("jamf policy has an invalid package reference")
			}
			refs.packages[id] = true
		}
	}
	if len(seen) != count {
		return refs, errors.New("jamf policy enumeration is incomplete")
	}
	prestages, err := c.listObjects(ctx, "/api/v3/computer-prestages", nil)
	if err != nil {
		return refs, err
	}
	for _, data := range prestages {
		var stage struct {
			ID               string    `json:"id"`
			CustomPackageIDs *[]string `json:"customPackageIds"`
		}
		if err := json.Unmarshal(data, &stage); err != nil || !validID(stage.ID) || stage.CustomPackageIDs == nil {
			return refs, errors.New("invalid Jamf PreStage response")
		}
		for _, id := range *stage.CustomPackageIDs {
			if !validID(id) {
				return refs, errors.New("invalid Jamf PreStage package reference")
			}
			refs.packages[id] = true
		}
	}
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).GetBytes(titlePath)
	if err := requestError(ctx, result, err); err != nil {
		return refs, err
	}
	var configurations *[]titles.ResourcePatchSoftwareTitleConfiguration
	if err := json.Unmarshal(data, &configurations); err != nil || configurations == nil {
		return refs, errors.New("jamf title enumeration returned an incomplete result")
	}

	for _, summary := range *configurations {
		if !validID(summary.ID) || refs.titles[summary.ID] != nil {
			return refs, errors.New("invalid or duplicated Jamf title ID")
		}
		title, err := c.getTitle(ctx, summary.ID)
		if err != nil {
			return refs, err
		}
		refs.titles[title.ID] = title
		for _, pkg := range title.Packages {
			if !validID(pkg.PackageID) || pkg.Version == "" {
				return refs, errors.New("invalid Jamf title package reference")
			}
		}
	}
	patchPolicies, err := c.listPatchPolicies(ctx, "")
	if err != nil {
		return refs, err
	}
	for _, policy := range patchPolicies {
		if !validID(policy.SoftwareTitleConfigurationID) || policy.PolicyTargetVersion == "" || refs.titles[policy.SoftwareTitleConfigurationID] == nil {
			return refs, errors.New("jamf patch policy title references are incomplete")
		}
		refs.targets[associationKey(policy.SoftwareTitleConfigurationID, policy.PolicyTargetVersion)] = true
	}
	return refs, nil
}

func associationKey(titleID, version string) string { return titleID + "\x00" + version }

func (c *client) uploadFailure(ctx context.Context, state *binding, content payload, adopt string, current *observed, cause error) error {
	rev := state.Revisions[content.sha256]
	// The active binding continues to identify the last successful publication;
	// retained staging records remain addressable through their revision entries.
	state.PackageID = state.Revisions[state.PayloadSHA256].PackageID
	if current == nil || stringField(current.Fields, "cloudTransferStatus") != "FAILED" && stringField(current.Fields, "cloudTransferStatus") != "ERROR" {
		return fmt.Errorf("%w; staging package %s retained: transfer outcome is not definitively failed", cause, rev.PackageID)
	}
	if err := c.removeFailedUpload(ctx, state, content, adopt); err != nil {
		return fmt.Errorf("%w; staging package %s retained: %w", cause, rev.PackageID, err)
	}
	delete(state.Revisions, content.sha256)
	return fmt.Errorf("%w; failed staging package %s deleted", cause, rev.PackageID)
}

func (c *client) removeFailedUpload(ctx context.Context, state *binding, content payload, adopt string) error {
	rev := state.Revisions[content.sha256]
	if !rev.Owned || adopt != "" {
		return errors.New("package was adopted or creation ownership is unknown")
	}
	if state.PayloadSHA256 == content.sha256 || state.Publications.Order[content.sha256] != 0 {
		return errors.New("package has a successful publication")
	}
	for digest, other := range state.Revisions {
		if digest != content.sha256 && other.PackageID == rev.PackageID {
			return errors.New("package is reused by another revision")
		}
	}
	for _, a := range state.Associations {
		if a.PackageID == rev.PackageID || a.PreviousPackageID == rev.PackageID {
			return errors.New("package has an owned patch association")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	refs, err := c.references(ctx)
	if err != nil {
		return fmt.Errorf("reference visibility is incomplete: %w", err)
	}
	for _, title := range refs.titles {
		for _, pkg := range title.Packages {
			refs.packages[pkg.PackageID] = true
		}
	}
	if refs.packages[rev.PackageID] {
		return errors.New("package is referenced by a policy, PreStage or patch title")
	}
	current, err := c.get(ctx, rev.PackageID)
	if err != nil {
		return fmt.Errorf("package identity could not be rechecked: %w", err)
	}
	if current == nil {
		return nil
	}
	status := stringField(current.Fields, "cloudTransferStatus")
	if stringField(current.Fields, "fileName") != content.filename || status != "FAILED" && status != "ERROR" {
		return errors.New("package identity or transfer status changed before deletion")
	}
	result, deleteErr := c.packages.DeleteByIDV1(ctx, rev.PackageID)
	deleteErr = requestError(ctx, result, deleteErr)
	remaining, err := c.get(ctx, rev.PackageID)
	if err != nil {
		return fmt.Errorf("deletion outcome could not be verified: %w", err)
	}
	if remaining != nil {
		if deleteErr != nil {
			return fmt.Errorf("deletion failed: %w", deleteErr)
		}
		return errors.New("package remains after deletion")
	}
	return nil
}

func (c *client) prune(ctx context.Context, state *binding, content payload, keep int, apply bool, response *plugin.ReconcileResponse) error {
	family, err := c.listObjects(ctx, packagePath, map[string]string{"filter": `fileName=="` + content.prefix + `*"`})
	if err != nil {
		return fmt.Errorf("jamf pruning blocked: %w", err)
	}
	history := state.Publications
	history.Order = maps.Clone(history.Order)
	if !apply {
		history.Record(content.sha256)
	}
	retained := history.Retained(keep)
	if len(retained) == 0 {
		return errors.New("jamf pruning blocked: publication order is unknown")
	}
	candidates := map[string]string{}
	observedIDs := map[string]bool{}
	for _, data := range family {
		fields, err := decodeObject(data)
		if err != nil {
			return err
		}
		filename, id := stringField(fields, "fileName"), stringField(fields, "id")
		if !strings.HasPrefix(filename, content.prefix) || !strings.HasSuffix(filename, ".pkg") {
			continue
		}
		digest := strings.TrimSuffix(strings.TrimPrefix(filename, content.prefix), ".pkg")
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 || !validID(id) {
			return errors.New("jamf pruning blocked: managed package identity is invalid")
		}
		observedIDs[id] = true
		if retained[digest] {
			continue
		}
		rev, ok := state.Revisions[digest]
		if !ok || !rev.Owned || rev.PackageID != id || history.Order[digest] == 0 {
			return errors.New("jamf pruning blocked: package ownership or publication order is unknown")
		}
		candidates[id] = digest
	}
	for digest, rev := range state.Revisions {
		if retained[digest] {
			continue
		}
		if !observedIDs[rev.PackageID] {
			pkg, err := c.get(ctx, rev.PackageID)
			if err != nil {
				return err
			}
			if pkg != nil {
				return errors.New("jamf pruning blocked: an owned package filename changed outside Stemma")
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	refs, err := c.references(ctx)
	if err != nil {
		return fmt.Errorf("jamf pruning blocked: reference visibility is incomplete: %w", err)
	}
	retiring := map[string]association{}
	for _, a := range state.Associations {
		if _, candidate := candidates[a.PackageID]; !candidate {
			continue
		}
		title := refs.titles[a.TitleID]
		if title == nil {
			return errors.New("jamf pruning blocked: an owned title association disappeared")
		}
		linked, err := titlePackage(title, a.Version)
		if err != nil {
			return err
		}
		if linked != a.PackageID {
			if (a.Pending || a.Removing) && linked == "" {
				continue
			}
			return errors.New("jamf pruning blocked: an owned patch association changed outside Stemma")
		}
		if refs.targets[associationKey(a.TitleID, a.Version)] {
			continue
		}
		retiring[associationKey(a.TitleID, a.Version)] = a
	}
	if apply {
		state.Associations = slices.DeleteFunc(state.Associations, func(a association) bool {
			if !a.Removing {
				return false
			}
			title := refs.titles[a.TitleID]
			if title == nil {
				return false
			}
			linked, err := titlePackage(title, a.Version)
			return err == nil && linked == ""
		})
	}
	for _, title := range refs.titles {
		for _, pkg := range title.Packages {
			a, retire := retiring[associationKey(title.ID, pkg.Version)]
			if !retire || a.PackageID != pkg.PackageID {
				refs.packages[pkg.PackageID] = true
			}
		}
	}
	// A protected package keeps its owned association too; retention never edits a
	// deployment merely to satisfy the requested package count.
	for key, a := range retiring {
		if refs.packages[a.PackageID] {
			delete(retiring, key)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(candidates)) {
		if refs.packages[id] {
			response.Changes = append(response.Changes, plugin.Change{Kind: "cleanup", Field: "package", Action: "retain_referenced", Before: raw(id)})
			continue
		}
		response.Changes = append(response.Changes, plugin.Change{Kind: "cleanup", Field: "package", Action: "delete", Before: raw(id)})
	}
	if !apply {
		return nil
	}
	for _, titleID := range slices.Sorted(maps.Keys(refs.titles)) {
		title := refs.titles[titleID]
		links := slices.DeleteFunc(slices.Clone(title.Packages), func(pkg titles.SubsetPackage) bool {
			a, ok := retiring[associationKey(titleID, pkg.Version)]
			return ok && a.PackageID == pkg.PackageID
		})
		if len(links) == len(title.Packages) {
			continue
		}
		fresh, err := c.getTitle(ctx, titleID)
		if err != nil {
			return err
		}
		if !sameAssociations(fresh.Packages, title.Packages) {
			return errors.New("jamf pruning blocked: title associations changed during cleanup")
		}
		for i := range state.Associations {
			a := &state.Associations[i]
			if _, ok := retiring[associationKey(a.TitleID, a.Version)]; ok && a.TitleID == titleID {
				a.Removing = true
			}
		}
		if err := c.setTitlePackages(ctx, titleID, links); err != nil {
			return err
		}
		state.Associations = slices.DeleteFunc(state.Associations, func(a association) bool {
			retired, ok := retiring[associationKey(a.TitleID, a.Version)]
			return ok && retired.PackageID == a.PackageID
		})
	}
	// Re-read every reference boundary after retiring links and before deleting
	// content. An administrator-created reference wins over an earlier plan.
	fresh, err := c.references(ctx)
	if err != nil {
		return fmt.Errorf("jamf pruning blocked: reference recheck failed: %w", err)
	}
	restored := false
	for _, key := range slices.Sorted(maps.Keys(retiring)) {
		a := retiring[key]
		if !fresh.targets[key] {
			continue
		}
		title := fresh.titles[a.TitleID]
		linked, err := titlePackage(title, a.Version)
		if err != nil {
			return err
		}
		if linked != "" && linked != a.PackageID {
			return errors.New("jamf pruning blocked: a new patch target has a changed association")
		}
		if linked == "" {
			a.Pending = true
			state.Associations = append(state.Associations, a)
			links := append(slices.Clone(title.Packages), titles.SubsetPackage{PackageID: a.PackageID, Version: a.Version})
			if err := c.setTitlePackages(ctx, a.TitleID, links); err != nil {
				return err
			}
			state.Associations[len(state.Associations)-1].Pending = false
		}
		restored = true
	}
	if restored {
		return errors.New("jamf pruning blocked: a new patch policy target appeared; its owned association was restored")
	}
	for _, title := range fresh.titles {
		for _, pkg := range title.Packages {
			fresh.packages[pkg.PackageID] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(candidates)) {
		if refs.packages[id] || fresh.packages[id] {
			for i := range response.Changes {
				change := &response.Changes[i]
				if change.Kind == "cleanup" && equalJSON(change.Before, raw(id)) {
					change.Action = "retain_referenced"
				}
			}
			continue
		}
		digest := candidates[id]
		current, err := c.get(ctx, id)
		if err != nil {
			return err
		}
		if current != nil {
			if stringField(current.Fields, "fileName") != content.prefix+digest+".pkg" || !contentDigestMatches(current, payload{sha256: digest, sha3512: state.Revisions[digest].SHA3512}) {
				return errors.New("jamf pruning blocked: package identity changed before deletion")
			}
			result, deleteErr := c.packages.DeleteByIDV1(ctx, id)
			deleteErr = requestError(ctx, result, deleteErr)
			actual, err := c.get(ctx, id)
			if err != nil || actual != nil {
				if deleteErr != nil {
					return deleteErr
				}
				return errors.New("jamf package deletion did not match readback")
			}
		}
		delete(state.Revisions, digest)
	}
	return nil
}

func sameAssociations(a, b []titles.SubsetPackage) bool {
	if len(a) != len(b) {
		return false
	}
	for _, link := range a {
		if !slices.ContainsFunc(b, func(other titles.SubsetPackage) bool {
			return other.Version == link.Version && other.PackageID == link.PackageID
		}) {
			return false
		}
	}
	return true
}

func (c *client) readXML(ctx context.Context, path, root string) (*xmlNode, error) {
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).GetBytes(path)
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	return parseXML(data, root)
}

func (c *client) listObjects(ctx context.Context, path string, query map[string]string) ([]json.RawMessage, error) {
	params := maps.Clone(query)
	if params == nil {
		params = map[string]string{}
	}
	params["page-size"], params["sort"] = "100", "id:asc"
	var objects []json.RawMessage
	expected := -1
	seen := map[string]bool{}
	for page := 0; ; page++ {
		params["page"] = strconv.Itoa(page)
		result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).SetQueryParams(params).GetBytes(path)
		if err := requestError(ctx, result, err); err != nil {
			return nil, err
		}
		var batch struct {
			TotalCount *int               `json:"totalCount"`
			Results    *[]json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(data, &batch); err != nil || batch.TotalCount == nil || *batch.TotalCount < 0 || batch.Results == nil {
			return nil, errors.New("jamf collection response is incomplete")
		}
		if expected >= 0 && expected != *batch.TotalCount {
			return nil, errors.New("jamf collection changed during enumeration")
		}
		expected = *batch.TotalCount
		for _, object := range *batch.Results {
			var row struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(object, &row); err != nil || !validID(row.ID) || seen[row.ID] {
				return nil, errors.New("jamf collection IDs are invalid or duplicated")
			}
			seen[row.ID] = true
		}
		objects = append(objects, (*batch.Results)...)
		if len(objects) == expected {
			return objects, nil
		}
		if len(*batch.Results) == 0 || len(objects) > expected {
			return nil, errors.New("jamf collection enumeration is incomplete")
		}
	}
}
