package jamf

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_policies"
	titles "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_software_title_configurations"
	"github.com/woodleighschool/stemma/plugin"
)

const titlePath = "/api/v3/patch-software-title-configurations"
const policyPath = "/JSSResource/patchpolicies"

type patchConfig struct {
	TitleConfigurationID string       `json:"title_configuration_id" jsonschema:"pattern=^[1-9][0-9]*$"`
	Version              string       `json:"version" jsonschema:"minLength=1"`
	Policy               *patchPolicy `json:"policy,omitempty"`
}

type patchPolicy struct {
	ID                 string             `json:"id,omitempty" jsonschema:"pattern=^[1-9][0-9]*$"`
	Name               *string            `json:"name,omitempty"`
	Enabled            *bool              `json:"enabled,omitempty"`
	DistributionMethod *string            `json:"distribution_method,omitempty" jsonschema:"enum=automatically,enum=selfservice"`
	AllowDowngrade     *bool              `json:"allow_downgrade,omitempty"`
	PatchUnknown       *bool              `json:"patch_unknown,omitempty"`
	Scope              *policyScope       `json:"scope,omitempty"`
	UserInteraction    *policyInteraction `json:"user_interaction,omitempty"`
}

type policyObject struct {
	ID int `json:"id" jsonschema:"minimum=1"`
}
type policyScope struct {
	AllComputers   *bool              `json:"all_computers,omitempty"`
	Computers      *[]policyObject    `json:"computers,omitempty"`
	ComputerGroups *[]policyObject    `json:"computer_groups,omitempty"`
	Buildings      *[]policyObject    `json:"buildings,omitempty"`
	Departments    *[]policyObject    `json:"departments,omitempty"`
	Limitations    *policyLimitations `json:"limitations,omitempty"`
	Exclusions     *policyExclusions  `json:"exclusions,omitempty"`
}
type policyLimitations struct {
	NetworkSegments *[]policyObject `json:"network_segments,omitempty"`
}
type policyExclusions struct {
	Computers       *[]policyObject `json:"computers,omitempty"`
	ComputerGroups  *[]policyObject `json:"computer_groups,omitempty"`
	Buildings       *[]policyObject `json:"buildings,omitempty"`
	Departments     *[]policyObject `json:"departments,omitempty"`
	NetworkSegments *[]policyObject `json:"network_segments,omitempty"`
}
type policyInteraction struct {
	InstallButtonText      *string              `json:"install_button_text,omitempty"`
	SelfServiceDescription *string              `json:"self_service_description,omitempty"`
	SelfServiceIcon        *policyObject        `json:"self_service_icon,omitempty"`
	Notifications          *policyNotifications `json:"notifications,omitempty"`
	Deadlines              *policyDeadlines     `json:"deadlines,omitempty"`
	GracePeriod            *policyGrace         `json:"grace_period,omitempty"`
}
type policyNotifications struct {
	Enabled   *bool            `json:"notification_enabled,omitempty"`
	Type      *string          `json:"notification_type,omitempty"`
	Subject   *string          `json:"notification_subject,omitempty"`
	Message   *string          `json:"notification_message,omitempty"`
	Reminders *policyReminders `json:"reminders,omitempty"`
}
type policyReminders struct {
	Enabled   *bool `json:"notification_reminders_enabled,omitempty"`
	Frequency *int  `json:"notification_reminder_frequency,omitempty" jsonschema:"minimum=0"`
}
type policyDeadlines struct {
	Enabled *bool `json:"deadline_enabled,omitempty"`
	Period  *int  `json:"deadline_period,omitempty" jsonschema:"minimum=0"`
}
type policyGrace struct {
	Duration *int    `json:"grace_period_duration,omitempty" jsonschema:"minimum=0"`
	Subject  *string `json:"notification_center_subject,omitempty"`
	Message  *string `json:"message,omitempty"`
}

// An association is owned only after creating it, with the exact tuple retained
// before the write so an ambiguous response cannot lose that ownership.
type association struct {
	TitleID           string `json:"title_id"`
	Version           string `json:"version"`
	PackageID         string `json:"package_id"`
	Pending           bool   `json:"pending,omitempty"`
	PreviousPackageID string `json:"previous_package_id,omitempty"`
	Removing          bool   `json:"removing,omitempty"`
}

func decodePatch(metadata map[string]json.RawMessage) (*patchConfig, error) {
	data, ok := metadata["patch"]
	if !ok {
		return nil, nil
	}
	if err := validatePatchJSON(data); err != nil {
		return nil, fmt.Errorf("jamf patch: %w", err)
	}
	var patch patchConfig
	if err := strictDecode(data, &patch); err != nil {
		return nil, fmt.Errorf("jamf patch: %w", err)
	}
	if !validID(patch.TitleConfigurationID) || strings.TrimSpace(patch.Version) == "" {
		return nil, errors.New("jamf patch requires title_configuration_id and an exact version")
	}
	if p := patch.Policy; p != nil {
		if p.ID != "" && !validID(p.ID) {
			return nil, errors.New("jamf patch policy id must be a positive numeric string")
		}
		if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
			return nil, errors.New("jamf patch policy name cannot be empty")
		}
		if p.DistributionMethod != nil && *p.DistributionMethod != "automatically" && *p.DistributionMethod != "selfservice" {
			return nil, errors.New("jamf patch distribution_method must be automatically or selfservice")
		}
	}
	return &patch, nil
}

func validatePatchJSON(data json.RawMessage) error {
	if string(data) == "null" {
		return errors.New("null is not supported")
	}
	if len(data) == 0 {
		return errors.New("empty JSON value")
	}
	switch data[0] {
	case '{':
		fields, err := decodeObject(data)
		if err != nil {
			return err
		}
		for _, value := range fields {
			if err := validatePatchJSON(value); err != nil {
				return err
			}
		}
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := validatePatchJSON(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *client) getTitle(ctx context.Context, id string) (*titles.ResourcePatchSoftwareTitleConfiguration, error) {
	result, response, err := titles.NewPatchSoftwareTitleConfigurations(c.transport).GetByIDV3(ctx, id)
	if err := requestError(ctx, response, err); err != nil {
		return nil, err
	}
	if result == nil || result.ID != id {
		return nil, errors.New("jamf title response does not match requested configuration")
	}
	fields, err := decodeObject(response.Bytes())
	if err != nil {
		return nil, err
	}
	if data := fields["packages"]; len(data) == 0 || data[0] != '[' {
		return nil, errors.New("jamf title package associations were omitted")
	}
	return result, nil
}

func titlePackage(title *titles.ResourcePatchSoftwareTitleConfiguration, version string) (string, error) {
	var found string
	for _, pkg := range title.Packages {
		if pkg.Version != version {
			continue
		}
		if found != "" {
			return "", errors.New("jamf title has multiple package associations for one version")
		}
		found = pkg.PackageID
	}
	return found, nil
}

func ownedAssociation(state *binding, titleID, version, packageID string) bool {
	return slices.ContainsFunc(state.Associations, func(a association) bool {
		return a.TitleID == titleID && a.Version == version && (a.PackageID == packageID || a.Pending && a.PreviousPackageID == packageID)
	})
}

func (c *client) checkPatch(ctx context.Context, patch *patchConfig, state *binding, response *plugin.ReconcileResponse) error {
	title, err := c.getTitle(ctx, patch.TitleConfigurationID)
	if err != nil {
		return fmt.Errorf("jamf patch title: %w", err)
	}
	definitions, result, err := titles.NewPatchSoftwareTitleConfigurations(c.transport).GetDefinitionsByIDV3(ctx, patch.TitleConfigurationID, nil)
	if err := requestError(ctx, result, err); err != nil {
		return err
	}
	if definitions == nil || !slices.ContainsFunc(definitions.Results, func(d titles.ResourceDefinition) bool { return d.Version == patch.Version }) {
		return errors.New("jamf patch title does not define the requested exact version")
	}
	linked, err := titlePackage(title, patch.Version)
	if err != nil {
		return err
	}
	if linked != "" && linked != state.PackageID && !ownedAssociation(state, patch.TitleConfigurationID, patch.Version, linked) {
		return errors.New("jamf patch version already references an administrator-owned package")
	}
	for _, a := range state.Associations {
		if a.TitleID == patch.TitleConfigurationID && a.Version == patch.Version && linked != a.PackageID && (!a.Pending || linked != a.PreviousPackageID) && (!a.Removing || linked != "") {
			return errors.New("jamf patch association changed outside Stemma")
		}
	}
	if linked == "" || linked != state.PackageID {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "patch.packages", Action: "associate", Before: raw(linked), After: raw(patch.Version)})
	}
	if patch.Policy == nil {
		return nil
	}
	if state.PolicyTitleID != "" && state.PolicyTitleID != patch.TitleConfigurationID {
		return errors.New("jamf patch policy binding belongs to a different title configuration")
	}
	if patch.Policy.ID != "" {
		if state.PolicyID != "" && state.PolicyID != patch.Policy.ID {
			return errors.New("jamf patch policy id conflicts with the durable binding")
		}
		state.PolicyID = patch.Policy.ID
	}
	policy, err := c.observePolicy(ctx, patch, state)
	if err != nil {
		return err
	}
	desired := desiredPolicy(patch, state, policy == nil)
	if policy == nil {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "patch.policy", Action: "create", After: raw(state.PolicyName)})
	} else if !containsXML(policy, desired) {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "patch.policy", Action: "update", After: raw(patch.Version)})
	}
	return nil
}

func (c *client) applyPatch(ctx context.Context, patch *patchConfig, state *binding) error {
	title, err := c.getTitle(ctx, patch.TitleConfigurationID)
	if err != nil {
		return err
	}
	linked, err := titlePackage(title, patch.Version)
	if err != nil {
		return err
	}
	if linked != state.PackageID {
		if linked != "" && !ownedAssociation(state, patch.TitleConfigurationID, patch.Version, linked) {
			return errors.New("jamf patch association changed before update")
		}
		a := association{TitleID: patch.TitleConfigurationID, Version: patch.Version, PackageID: state.PackageID, Pending: true, PreviousPackageID: linked}
		state.Associations = slices.DeleteFunc(state.Associations, func(old association) bool { return old.TitleID == a.TitleID && old.Version == a.Version })
		state.Associations = append(state.Associations, a)
		links := slices.DeleteFunc(slices.Clone(title.Packages), func(p titles.SubsetPackage) bool { return p.Version == patch.Version })
		links = append(links, titles.SubsetPackage{PackageID: state.PackageID, Version: patch.Version})
		if err := c.setTitlePackages(ctx, title.ID, links); err != nil {
			return err
		}
	}
	if ownedAssociation(state, patch.TitleConfigurationID, patch.Version, state.PackageID) {
		state.Associations = slices.DeleteFunc(state.Associations, func(a association) bool { return a.TitleID == patch.TitleConfigurationID && a.Version == patch.Version })
		state.Associations = append(state.Associations, association{TitleID: patch.TitleConfigurationID, Version: patch.Version, PackageID: state.PackageID})
	}
	if patch.Policy == nil {
		return nil
	}
	policy, err := c.observePolicy(ctx, patch, state)
	if err != nil {
		return err
	}
	if policy == nil {
		if state.PendingPolicy {
			return errors.New("jamf patch policy creation remains unresolved")
		}
		state.PendingPolicy, state.PolicyTitleID = true, patch.TitleConfigurationID
		initial := desiredPolicy(patch, state, true)
		general := initial.child("general")
		general.set(xmlNode{XMLName: xml.Name{Local: "enabled"}, Text: "false"})
		initial.set(xmlNode{XMLName: xml.Name{Local: "scope"}, Children: []xmlNode{{XMLName: xml.Name{Local: "all_computers"}, Text: "false"}}})
		body, err := xml.Marshal(initial)
		if err != nil {
			return err
		}
		result, createErr := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Post(policyPath + "/softwaretitleconfig/id/" + patch.TitleConfigurationID)
		createErr = requestError(ctx, result, createErr)
		if createErr == nil {
			created, err := parseXML(result.Bytes(), "patch_policy")
			if err == nil {
				state.PolicyID = created.value("id")
				if state.PolicyID == "" {
					state.PolicyID = created.value("general", "id")
				}
			}
			if !validID(state.PolicyID) {
				state.PolicyID = ""
			}
		}
		policy, err = c.observePolicy(ctx, patch, state)
		if err != nil || policy == nil {
			return errors.New("jamf patch policy create outcome unresolved; no create retry was sent")
		}
	}
	desired := desiredPolicy(patch, state, false)
	if containsXML(policy, desired) {
		return nil
	}
	mergeXML(policy, desired)
	body, err := xml.Marshal(policy)
	if err != nil {
		return err
	}
	result, putErr := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Put(policyPath + "/id/" + state.PolicyID)
	putErr = requestError(ctx, result, putErr)
	actual, err := c.getPolicy(ctx, state.PolicyID)
	if err != nil || actual == nil || !containsXML(actual, desired) {
		if putErr != nil {
			return putErr
		}
		return errors.New("jamf patch policy update did not match readback")
	}
	return nil
}

func (c *client) setTitlePackages(ctx context.Context, id string, links []titles.SubsetPackage) error {
	// The SDK's update model emits unrelated required and response fields.
	body := make([]map[string]string, 0, len(links))
	for _, link := range links {
		body = append(body, map[string]string{"packageId": link.PackageID, "version": link.Version})
	}
	result, updateErr := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).SetHeader("Content-Type", constants.ApplicationJSON).SetBody(map[string]any{"packages": body}).DisableRetry().Patch(titlePath + "/" + id)
	updateErr = requestError(ctx, result, updateErr)
	actual, err := c.getTitle(ctx, id)
	if err != nil {
		return err
	}
	if len(actual.Packages) != len(links) {
		return errors.New("jamf patch package associations did not match readback")
	}
	for _, link := range links {
		found, err := titlePackage(actual, link.Version)
		if err != nil || found != link.PackageID {
			if updateErr != nil {
				return updateErr
			}
			return errors.New("jamf patch package association did not match readback")
		}
	}
	return nil
}

func (c *client) listPatchPolicies(ctx context.Context, filter string) ([]patch_policies.ResourcePatchPolicySummary, error) {
	objects, err := c.listObjects(ctx, "/api/v2/patch-policies", map[string]string{"filter": filter})
	if err != nil {
		return nil, err
	}
	policies := make([]patch_policies.ResourcePatchPolicySummary, 0, len(objects))
	for _, data := range objects {
		var policy patch_policies.ResourcePatchPolicySummary
		if err := json.Unmarshal(data, &policy); err != nil {
			return nil, errors.New("invalid Jamf patch policy summary")
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

func (c *client) observePolicy(ctx context.Context, patch *patchConfig, state *binding) (*xmlNode, error) {
	if state.PolicyName == "" {
		state.PolicyName = "Stemma " + state.IdentitySHA256 + " patch"
		if patch.Policy.Name != nil {
			state.PolicyName = *patch.Policy.Name
		}
	}
	if state.PolicyID == "" {
		policies, err := c.listPatchPolicies(ctx, "softwareTitleConfigurationId=="+patch.TitleConfigurationID)
		if err != nil {
			return nil, err
		}
		for _, p := range policies {
			if p.PolicyName != state.PolicyName {
				continue
			}
			if !state.PendingPolicy {
				return nil, errors.New("jamf patch policy name already exists; use patch.policy.id to adopt it")
			}
			if state.PolicyID != "" {
				return nil, errors.New("multiple Jamf patch policies match the pending creation")
			}
			state.PolicyID = p.ID
		}
		if state.PolicyID == "" {
			return nil, nil
		}
	}
	policy, err := c.getPolicy(ctx, state.PolicyID)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, errors.New("bound Jamf patch policy no longer exists")
	}
	if policy.value("software_title_configuration_id") != patch.TitleConfigurationID {
		return nil, errors.New("jamf patch policy belongs to a different title configuration")
	}
	state.PendingPolicy = false
	state.PolicyTitleID = patch.TitleConfigurationID
	return policy, nil
}

func (c *client) getPolicy(ctx context.Context, id string) (*xmlNode, error) {
	if !validID(id) {
		return nil, errors.New("invalid Jamf patch policy ID")
	}
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).GetBytes(policyPath + "/id/" + id)
	if result != nil && result.StatusCode() == 404 {
		return nil, nil
	}
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	policy, err := parseXML(data, "patch_policy")
	if err != nil {
		return nil, err
	}
	actual := policy.value("general", "id")
	if actual == "" {
		actual = policy.value("id")
	}
	if actual != id {
		return nil, errors.New("jamf patch policy response ID does not match request")
	}
	return policy, nil
}

// xmlNode retains unowned Classic API fields during partial native reconciliation.
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []xmlNode  `xml:",any"`
	Replace  bool       `xml:"-"`
}

func parseXML(data []byte, root string) (*xmlNode, error) {
	var node xmlNode
	if err := xml.Unmarshal(data, &node); err != nil {
		return nil, errors.New("invalid Jamf XML response")
	}
	if node.XMLName.Local != root {
		return nil, errors.New("unexpected Jamf XML response root")
	}
	return &node, nil
}
func (n *xmlNode) child(name string) *xmlNode {
	if n == nil {
		return nil
	}
	for i := range n.Children {
		if n.Children[i].XMLName.Local == name {
			return &n.Children[i]
		}
	}
	return nil
}
func (n *xmlNode) value(path ...string) string {
	for _, name := range path {
		n = n.child(name)
		if n == nil {
			return ""
		}
	}
	return strings.TrimSpace(n.Text)
}
func (n *xmlNode) set(value xmlNode) {
	if child := n.child(value.XMLName.Local); child != nil {
		*child = value
		return
	}
	n.Children = append(n.Children, value)
}
func containsXML(actual, desired *xmlNode) bool {
	if actual == nil {
		return false
	}
	if desired.Replace {
		return equalXML(actual, desired)
	}
	if len(desired.Children) == 0 {
		return actual.Text == desired.Text
	}
	for i := range desired.Children {
		child := &desired.Children[i]
		if !containsXML(actual.child(child.XMLName.Local), child) {
			return false
		}
	}
	return true
}
func equalXML(a, b *xmlNode) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.XMLName.Local != b.XMLName.Local || len(a.Children) != len(b.Children) {
		return false
	}
	if len(a.Children) == 0 {
		return strings.TrimSpace(a.Text) == strings.TrimSpace(b.Text)
	}
	for i := range a.Children {
		if !equalXML(&a.Children[i], &b.Children[i]) {
			return false
		}
	}
	return true
}
func mergeXML(current, desired *xmlNode) {
	for _, wanted := range desired.Children {
		old := current.child(wanted.XMLName.Local)
		if old == nil || wanted.Replace || len(wanted.Children) == 0 {
			current.set(wanted)
		} else {
			mergeXML(old, &wanted)
		}
	}
}
func desiredPolicy(patch *patchConfig, state *binding, create bool) *xmlNode {
	fields, _ := decodeObject(raw(patch.Policy))
	delete(fields, "id")
	general := make(map[string]json.RawMessage)
	for name, value := range fields {
		if name != "scope" && name != "user_interaction" {
			general[name] = value
			delete(fields, name)
		}
	}
	general["target_version"] = raw(patch.Version)
	if create {
		if _, ok := general["name"]; !ok {
			general["name"] = raw(state.PolicyName)
		}
		if _, ok := general["enabled"]; !ok {
			general["enabled"] = raw(false)
		}
		if _, ok := general["distribution_method"]; !ok {
			general["distribution_method"] = raw("automatically")
		}
		if _, ok := fields["scope"]; !ok {
			fields["scope"] = raw(map[string]bool{"all_computers": false})
		}
	}
	fields["general"] = raw(general)
	fields["software_title_configuration_id"] = raw(patch.TitleConfigurationID)
	return jsonXML("patch_policy", raw(fields))
}
func jsonXML(name string, data json.RawMessage) *xmlNode {
	n := &xmlNode{XMLName: xml.Name{Local: name}}
	switch data[0] {
	case '{':
		fields, _ := decodeObject(data)
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			n.Children = append(n.Children, *jsonXML(key, fields[key]))
		}
	case '[':
		n.Replace = true
		var entries []json.RawMessage
		_ = json.Unmarshal(data, &entries)
		singular := strings.TrimSuffix(name, "s")
		for _, entry := range entries {
			n.Children = append(n.Children, *jsonXML(singular, entry))
		}
	case '"':
		_ = json.Unmarshal(data, &n.Text)
	default:
		n.Text = string(data)
	}
	return n
}
