package jamf

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
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

// retired returns the family members beyond the current record and the keep-1
// newest others. Jamf packages carry no timestamp; numeric IDs are their
// creation order.
func retired(family []summary, current string, keep int) []summary {
	others := slices.DeleteFunc(slices.Clone(family), func(pkg summary) bool { return pkg.id() == current })
	slices.SortFunc(others, func(a, b summary) int { return cmp.Compare(b.ID, a.ID) })
	return others[min(keep-1, len(others)):]
}

// prune deletes the retired packages that nothing in Jamf references; plan
// reports the same deletions.
func (c *client) prune(ctx context.Context, retiring []summary, patch *patchConfig, apply bool, response *plugin.ReconcileResponse) error {
	if len(retiring) == 0 {
		return nil
	}
	referenced, err := c.references(ctx, patch)
	if err != nil {
		return fmt.Errorf("jamf retention blocked: references could not be read: %w", err)
	}
	for _, pkg := range retiring {
		id := pkg.id()
		if referenced[id] {
			plugin.Logger(ctx).DebugContext(ctx, "Referenced package retained", "package", id, "file_name", pkg.FileName)
			continue
		}
		if apply {
			result, err := c.packages.DeleteByIDV1(ctx, id)
			// A delete retried after a lost response finds the package already gone.
			gone := result != nil && result.StatusCode() == http.StatusNotFound
			if err := requestError(ctx, result, err); err != nil && !gone {
				return fmt.Errorf("jamf retention: delete package %s: %w", id, err)
			}
		}
		response.Changes = append(response.Changes, plugin.Change{Kind: "retention", Field: pkg.FileName, Action: "delete", Before: raw(id)})
	}
	return nil
}

// references returns the packages a policy, PreStage or patch title uses. Each
// enumeration must be complete, because a package missing from a partial read
// may still be deployed.
func (c *client) references(ctx context.Context, patch *patchConfig) (map[string]bool, error) {
	referenced := map[string]bool{}
	policies, err := c.readXML(ctx, "/JSSResource/policies", "policies")
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(policies.value("size"))
	if err != nil || count < 0 {
		return nil, errors.New("jamf policy enumeration has no valid count")
	}
	seen := map[string]bool{}
	for _, entry := range policies.Children {
		if entry.XMLName.Local != "policy" {
			continue
		}
		id := entry.value("id")
		if !validID(id) || seen[id] {
			return nil, errors.New("jamf policy enumeration contains invalid or duplicate IDs")
		}
		seen[id] = true
		policy, err := c.readXML(ctx, "/JSSResource/policies/id/"+id, "policy")
		if err != nil {
			return nil, err
		}
		if policy.value("general", "id") != id {
			return nil, errors.New("jamf policy response ID does not match request")
		}
		installs := policy.child("package_configuration").child("packages")
		if installs == nil {
			return nil, errors.New("jamf policy package references were omitted")
		}
		for _, pkg := range installs.Children {
			if pkg.XMLName.Local != "package" {
				continue
			}
			id := pkg.value("id")
			if !validID(id) {
				return nil, errors.New("jamf policy has an invalid package reference")
			}
			referenced[id] = true
		}
	}
	if len(seen) != count {
		return nil, errors.New("jamf policy enumeration is incomplete")
	}
	prestages, err := c.listObjects(ctx, "/api/v3/computer-prestages", nil)
	if err != nil {
		return nil, err
	}
	for _, data := range prestages {
		var stage struct {
			ID               string    `json:"id"`
			CustomPackageIDs *[]string `json:"customPackageIds"`
		}
		if err := json.Unmarshal(data, &stage); err != nil || !validID(stage.ID) || stage.CustomPackageIDs == nil {
			return nil, errors.New("invalid Jamf PreStage response")
		}
		for _, id := range *stage.CustomPackageIDs {
			if !validID(id) {
				return nil, errors.New("invalid Jamf PreStage package reference")
			}
			referenced[id] = true
		}
	}
	configurations, err := c.listTitles(ctx)
	if err != nil {
		return nil, err
	}
	for _, listed := range configurations {
		title, err := c.getTitle(ctx, listed.ID)
		if err != nil {
			return nil, err
		}
		for _, pkg := range title.Packages {
			if !validID(pkg.PackageID) || pkg.Version == "" {
				return nil, errors.New("invalid Jamf title package reference")
			}
			// The declared association ends at the current record, whichever
			// package holds it while planning.
			if patch != nil && title.ID == patch.titleID && pkg.Version == patch.version {
				continue
			}
			referenced[pkg.PackageID] = true
		}
	}
	return referenced, nil
}

func (c *client) readXML(ctx context.Context, path, root string) (*xmlNode, error) {
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).GetBytes(path)
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	return parseXML(data, root)
}
