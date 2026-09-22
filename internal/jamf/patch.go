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

// patchConfig declares patch deployment: an existing title, found by its
// display name, and an optional patch policy. The version it deploys is the
// software's managed version, which the title's definitions must contain.
type patchConfig struct {
	Title  string       `json:"title" jsonschema:"minLength=1" jsonschema_description:"Display name of an existing patch software title. It must match exactly one title, and the title must define the software's managed version."`
	Policy *patchPolicy `json:"policy,omitempty" jsonschema_description:"Patch policy found by name within the title, where the name defaults to the software name. It is updated in place, or created disabled and unscoped before the declared settings are applied."`

	// titleID, version and scope are the title, the prepared version and the
	// scope object IDs, resolved against Jamf before planning.
	titleID string
	version string
	scope   map[string][]string
}

type patchPolicy struct {
	Name            *string            `json:"name,omitempty" jsonschema:"minLength=1" jsonschema_description:"Policy name within the title. Defaults to the software name."`
	Enabled         *bool              `json:"enabled,omitempty"`
	Distribution    *string            `json:"distribution,omitempty" jsonschema:"enum=automatic,enum=self_service" jsonschema_description:"Install automatically, or offer the update in Self Service."`
	AllowDowngrade  *bool              `json:"allow_downgrade,omitempty"`
	PatchUnknown    *bool              `json:"patch_unknown,omitempty" jsonschema_description:"Also patch computers with an unknown installed version."`
	Scope           *policyScope       `json:"scope,omitempty" jsonschema_description:"Scope objects named exactly as in Jamf. Each name must match exactly one object, and supplied lists replace their collections."`
	UserInteraction *policyInteraction `json:"user_interaction,omitempty"`
}

type policyScope struct {
	AllComputers   *bool              `json:"all_computers,omitempty"`
	Computers      *[]string          `json:"computers,omitempty"`
	ComputerGroups *[]string          `json:"computer_groups,omitempty"`
	Buildings      *[]string          `json:"buildings,omitempty"`
	Departments    *[]string          `json:"departments,omitempty"`
	Limitations    *policyLimitations `json:"limitations,omitempty"`
	Exclusions     *policyExclusions  `json:"exclusions,omitempty"`
}
type policyLimitations struct {
	NetworkSegments *[]string `json:"network_segments,omitempty"`
}
type policyExclusions struct {
	Computers       *[]string `json:"computers,omitempty"`
	ComputerGroups  *[]string `json:"computer_groups,omitempty"`
	Buildings       *[]string `json:"buildings,omitempty"`
	Departments     *[]string `json:"departments,omitempty"`
	NetworkSegments *[]string `json:"network_segments,omitempty"`
}
type policyInteraction struct {
	InstallButtonText      *string              `json:"install_button_text,omitempty"`
	SelfServiceDescription *string              `json:"self_service_description,omitempty"`
	SelfServiceIconID      *int                 `json:"self_service_icon_id,omitempty" jsonschema:"minimum=1" jsonschema_description:"ID of an icon uploaded to Jamf; icons have no unique name."`
	Notifications          *policyNotifications `json:"notifications,omitempty"`
	Deadlines              *policyDeadlines     `json:"deadlines,omitempty"`
	GracePeriod            *policyGrace         `json:"grace_period,omitempty"`
}
type policyNotifications struct {
	Enabled   *bool            `json:"enabled,omitempty"`
	Type      *string          `json:"type,omitempty"`
	Subject   *string          `json:"subject,omitempty"`
	Message   *string          `json:"message,omitempty"`
	Reminders *policyReminders `json:"reminders,omitempty"`
}
type policyReminders struct {
	Enabled   *bool `json:"enabled,omitempty"`
	Frequency *int  `json:"frequency,omitempty" jsonschema:"minimum=0" jsonschema_description:"Days between reminders."`
}
type policyDeadlines struct {
	Enabled *bool `json:"enabled,omitempty"`
	Period  *int  `json:"period,omitempty" jsonschema:"minimum=0" jsonschema_description:"Days until the deadline."`
}
type policyGrace struct {
	Duration *int    `json:"duration,omitempty" jsonschema:"minimum=0" jsonschema_description:"Minutes before a forced restart."`
	Subject  *string `json:"subject,omitempty"`
	Message  *string `json:"message,omitempty"`
}

var distributions = map[string]string{"automatic": "automatically", "self_service": "selfservice"}

// A scopeList is one scope collection and the kind of object its names select.
type scopeList struct {
	kind  string
	names *[]string
}

// scopeLists returns the declared scope collections by their Classic API path.
func (p *patchPolicy) scopeLists() map[string]scopeList {
	lists := map[string]scopeList{}
	s := p.Scope
	if s == nil {
		return lists
	}
	lists["computers"] = scopeList{"computer", s.Computers}
	lists["computer_groups"] = scopeList{"computer group", s.ComputerGroups}
	lists["buildings"] = scopeList{"building", s.Buildings}
	lists["departments"] = scopeList{"department", s.Departments}
	if l := s.Limitations; l != nil {
		lists["limitations/network_segments"] = scopeList{"network segment", l.NetworkSegments}
	}
	if e := s.Exclusions; e != nil {
		lists["exclusions/computers"] = scopeList{"computer", e.Computers}
		lists["exclusions/computer_groups"] = scopeList{"computer group", e.ComputerGroups}
		lists["exclusions/buildings"] = scopeList{"building", e.Buildings}
		lists["exclusions/departments"] = scopeList{"department", e.Departments}
		lists["exclusions/network_segments"] = scopeList{"network segment", e.NetworkSegments}
	}
	return lists
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
	if strings.TrimSpace(patch.Title) == "" {
		return nil, errors.New("jamf patch requires the title's display name")
	}
	if p := patch.Policy; p != nil {
		if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
			return nil, errors.New("jamf patch policy name cannot be empty")
		}
		if p.Distribution != nil && distributions[*p.Distribution] == "" {
			return nil, errors.New("jamf patch distribution must be automatic or self_service")
		}
		for path, list := range p.scopeLists() {
			if list.names != nil && slices.ContainsFunc(*list.names, func(name string) bool { return strings.TrimSpace(name) == "" }) {
				return nil, fmt.Errorf("jamf patch scope %s names cannot be empty", path)
			}
		}
	}
	return &patch, nil
}

// resolvePatch finds the title and scope objects the declaration names and
// takes the version the prepared installer supplies.
func (c *client) resolvePatch(ctx context.Context, patch *patchConfig, version string) error {
	if version == "" {
		return errors.New("jamf patch needs the prepared installer's managed version; select an application")
	}
	id, err := c.titleID(ctx, patch.Title)
	if err != nil {
		return err
	}
	patch.titleID, patch.version, patch.scope = id, version, map[string][]string{}
	if patch.Policy == nil {
		return nil
	}
	for path, list := range patch.Policy.scopeLists() {
		if list.names == nil {
			continue
		}
		ids := make([]string, 0, len(*list.names))
		for _, name := range slices.Compact(slices.Sorted(slices.Values(*list.names))) {
			id, err := c.objectID(ctx, list.kind, name)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		patch.scope[path] = ids
	}
	return nil
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
		return nil, errors.New("jamf title response does not match requested ID")
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

// policyName identifies the patch policy within its title.
func policyName(patch *patchConfig, software string) string {
	if patch.Policy.Name != nil {
		return *patch.Policy.Name
	}
	return software
}

// planPatch validates the declared version and reports the association and
// policy changes. packageID is empty while the package has yet to be created.
func (c *client) planPatch(ctx context.Context, patch *patchConfig, packageID, software string, response *plugin.ReconcileResponse) error {
	title, err := c.getTitle(ctx, patch.titleID)
	if err != nil {
		return fmt.Errorf("jamf patch title: %w", err)
	}
	definitions, result, err := titles.NewPatchSoftwareTitleConfigurations(c.transport).GetDefinitionsByIDV3(ctx, patch.titleID, nil)
	if err := requestError(ctx, result, err); err != nil {
		return err
	}
	if definitions == nil || !slices.ContainsFunc(definitions.Results, func(d titles.ResourceDefinition) bool { return d.Version == patch.version }) {
		var available []titles.ResourceDefinition
		if definitions != nil {
			available = definitions.Results
		}
		return fmt.Errorf("jamf patch title %q has no definition for %s%s", patch.Title, patch.version, recentDefinitions(available))
	}
	linked, err := titlePackage(title, patch.version)
	if err != nil {
		return err
	}
	if linked == "" || linked != packageID {
		change := plugin.Change{Kind: "metadata", Field: "patch.packages", Action: "associate"}
		if linked != "" {
			change.Before = raw(linked)
		}
		if packageID != "" {
			change.After = raw(packageID)
		}
		response.Changes = append(response.Changes, change)
	}
	if patch.Policy == nil {
		return nil
	}
	name := policyName(patch, software)
	_, policy, err := c.observePolicy(ctx, patch, name)
	if err != nil {
		return err
	}
	if policy == nil {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "patch.policy", Action: "create", After: raw(name)})
	} else if !containsXML(policy, desiredPolicy(patch, name, false)) {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "patch.policy", Action: "update", After: raw(patch.version)})
	}
	return nil
}

func (c *client) applyPatch(ctx context.Context, patch *patchConfig, packageID, software string) error {
	title, err := c.getTitle(ctx, patch.titleID)
	if err != nil {
		return err
	}
	linked, err := titlePackage(title, patch.version)
	if err != nil {
		return err
	}
	if linked != packageID {
		links := slices.DeleteFunc(slices.Clone(title.Packages), func(p titles.SubsetPackage) bool { return p.Version == patch.version })
		links = append(links, titles.SubsetPackage{PackageID: packageID, Version: patch.version})
		if err := c.setTitlePackages(ctx, title.ID, links); err != nil {
			return err
		}
	}
	if patch.Policy == nil {
		return nil
	}
	name := policyName(patch, software)
	id, policy, err := c.observePolicy(ctx, patch, name)
	if err != nil {
		return err
	}
	if policy == nil {
		if id, policy, err = c.createPolicy(ctx, patch, name); err != nil {
			return err
		}
	}
	desired := desiredPolicy(patch, name, false)
	if containsXML(policy, desired) {
		return nil
	}
	mergeXML(policy, desired)
	body, err := xml.Marshal(policy)
	if err != nil {
		return err
	}
	result, putErr := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Put(policyPath + "/id/" + id)
	putErr = requestError(ctx, result, putErr)
	actual, err := c.getPolicy(ctx, id)
	if err != nil || actual == nil || !containsXML(actual, desired) {
		if putErr != nil {
			return putErr
		}
		return errors.New("jamf patch policy update did not match readback")
	}
	return nil
}

// createPolicy creates the policy disabled and unscoped; the declared settings
// follow as an update, so a policy is never live before it is complete.
func (c *client) createPolicy(ctx context.Context, patch *patchConfig, name string) (string, *xmlNode, error) {
	initial := desiredPolicy(patch, name, true)
	initial.child("general").set(xmlNode{XMLName: xml.Name{Local: "enabled"}, Text: "false"})
	initial.set(xmlNode{XMLName: xml.Name{Local: "scope"}, Children: []xmlNode{{XMLName: xml.Name{Local: "all_computers"}, Text: "false"}}})
	body, err := xml.Marshal(initial)
	if err != nil {
		return "", nil, err
	}
	result, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Post(policyPath + "/softwaretitleconfig/id/" + patch.titleID)
	if createErr := requestError(ctx, result, err); createErr != nil {
		// A lost response can hide a policy Jamf did create; its name finds it.
		id, policy, err := c.observePolicy(ctx, patch, name)
		if err != nil || policy == nil {
			return "", nil, createErr
		}
		return id, policy, nil
	}
	created, err := parseXML(result.Bytes(), "patch_policy")
	if err != nil {
		return "", nil, err
	}
	id := created.value("id")
	if id == "" {
		id = created.value("general", "id")
	}
	policy, err := c.getPolicy(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if policy == nil {
		return "", nil, errors.New("created Jamf patch policy could not be read back")
	}
	return id, policy, nil
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
	if !sameAssociations(actual.Packages, links) {
		if updateErr != nil {
			return updateErr
		}
		return errors.New("jamf patch package associations did not match readback")
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

// observePolicy finds the policy by name within the title. It returns a nil
// policy when that name is free.
func (c *client) observePolicy(ctx context.Context, patch *patchConfig, name string) (string, *xmlNode, error) {
	policies, err := c.listPatchPolicies(ctx, "softwareTitleConfigurationId=="+patch.titleID)
	if err != nil {
		return "", nil, err
	}
	id := ""
	for _, p := range policies {
		if p.SoftwareTitleConfigurationID != patch.titleID || p.PolicyName != name {
			continue
		}
		if id != "" {
			return "", nil, fmt.Errorf("multiple Jamf patch policies are named %q in patch title %q; policy names must be unique", name, patch.Title)
		}
		id = p.ID
	}
	if id == "" {
		return "", nil, nil
	}
	policy, err := c.getPolicy(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if policy == nil {
		return "", nil, fmt.Errorf("jamf patch policy %s does not exist", id)
	}
	if policy.value("software_title_configuration_id") != patch.titleID {
		return "", nil, errors.New("jamf patch policy belongs to a different patch title")
	}
	return id, policy, nil
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
	XMLName  xml.Name   `xml:""`
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
		return sameItems(actual, desired)
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

// sameItems reports whether a replaced collection holds exactly the desired
// items in any order. Jamf returns more of each object than a declaration
// sets, such as its name beside its ID, and can add a size element.
func sameItems(actual, desired *xmlNode) bool {
	item := strings.TrimSuffix(desired.XMLName.Local, "s")
	var items []*xmlNode
	for i := range actual.Children {
		if actual.Children[i].XMLName.Local == item {
			items = append(items, &actual.Children[i])
		}
	}
	if len(items) != len(desired.Children) {
		return false
	}
	used := make([]bool, len(items))
	for i := range desired.Children {
		found := false
		for j, candidate := range items {
			if !used[j] && containsXML(candidate, &desired.Children[i]) {
				used[j], found = true, true
				break
			}
		}
		if !found {
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

// desiredPolicy is the Classic API patch policy the declaration describes,
// with scope objects as their resolved IDs. Creation fills the fields a new
// policy needs: disabled, unscoped and distributed automatically.
func desiredPolicy(patch *patchConfig, name string, create bool) *xmlNode {
	p := patch.Policy
	general := map[string]any{"target_version": patch.version}
	put(general, "name", p.Name)
	put(general, "enabled", p.Enabled)
	if p.Distribution != nil {
		general["distribution_method"] = distributions[*p.Distribution]
	}
	put(general, "allow_downgrade", p.AllowDowngrade)
	put(general, "patch_unknown", p.PatchUnknown)
	policy := map[string]any{"general": general, "software_title_configuration_id": patch.titleID}
	if p.Scope != nil {
		scope := map[string]any{}
		put(scope, "all_computers", p.Scope.AllComputers)
		for path, list := range p.scopeLists() {
			if list.names == nil {
				continue
			}
			objects := make([]map[string]string, 0, len(patch.scope[path]))
			for _, id := range patch.scope[path] {
				objects = append(objects, map[string]string{"id": id})
			}
			target := scope
			if group, key, nested := strings.Cut(path, "/"); nested {
				if _, exists := scope[group]; !exists {
					scope[group] = map[string]any{}
				}
				target, path = scope[group].(map[string]any), key
			}
			target[path] = objects
		}
		policy["scope"] = scope
	}
	if i := p.UserInteraction; i != nil {
		interaction := map[string]any{}
		put(interaction, "install_button_text", i.InstallButtonText)
		put(interaction, "self_service_description", i.SelfServiceDescription)
		if i.SelfServiceIconID != nil {
			interaction["self_service_icon"] = map[string]int{"id": *i.SelfServiceIconID}
		}
		if n := i.Notifications; n != nil {
			notifications := map[string]any{}
			put(notifications, "notification_enabled", n.Enabled)
			put(notifications, "notification_type", n.Type)
			put(notifications, "notification_subject", n.Subject)
			put(notifications, "notification_message", n.Message)
			if r := n.Reminders; r != nil {
				reminders := map[string]any{}
				put(reminders, "notification_reminders_enabled", r.Enabled)
				put(reminders, "notification_reminder_frequency", r.Frequency)
				notifications["reminders"] = reminders
			}
			interaction["notifications"] = notifications
		}
		if d := i.Deadlines; d != nil {
			deadlines := map[string]any{}
			put(deadlines, "deadline_enabled", d.Enabled)
			put(deadlines, "deadline_period", d.Period)
			interaction["deadlines"] = deadlines
		}
		if g := i.GracePeriod; g != nil {
			grace := map[string]any{}
			put(grace, "grace_period_duration", g.Duration)
			put(grace, "notification_center_subject", g.Subject)
			put(grace, "message", g.Message)
			interaction["grace_period"] = grace
		}
		policy["user_interaction"] = interaction
	}
	if create {
		if _, named := general["name"]; !named {
			general["name"] = name
		}
		if _, set := general["enabled"]; !set {
			general["enabled"] = false
		}
		if _, set := general["distribution_method"]; !set {
			general["distribution_method"] = "automatically"
		}
		if _, scoped := policy["scope"]; !scoped {
			policy["scope"] = map[string]bool{"all_computers": false}
		}
	}
	return jsonXML("patch_policy", raw(policy))
}

// put sets a declared value; an omitted one stays unmanaged.
func put[T any](fields map[string]any, key string, value *T) {
	if value != nil {
		fields[key] = *value
	}
}

// recentDefinitions names the newest definitions a title offers, for an error
// about a version it lacks.
func recentDefinitions(definitions []titles.ResourceDefinition) string {
	if len(definitions) == 0 {
		return ""
	}
	sorted := slices.Clone(definitions)
	slices.SortStableFunc(sorted, func(a, b titles.ResourceDefinition) int { return strings.Compare(b.ReleaseDate, a.ReleaseDate) })
	versions := make([]string, 0, 5)
	for _, definition := range sorted[:min(5, len(sorted))] {
		versions = append(versions, definition.Version)
	}
	return "; recent definitions: " + strings.Join(versions, ", ")
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
