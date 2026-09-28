package intune

import (
	"maps"

	abs "github.com/microsoft/kiota-abstractions-go"
)

func (c *client) apps() *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().BaseRequestBuilder
}

func (c *client) app(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).BaseRequestBuilder
}

func (c *client) assignments(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).Assignments().BaseRequestBuilder
}

func (c *client) assign(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).Assign().BaseRequestBuilder
}

func (c *client) relationships(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).Relationships().BaseRequestBuilder
}

func (c *client) updateRelationships(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).UpdateRelationships().BaseRequestBuilder
}

// Every app type shares the content protocol under its own type cast.
func (c *client) content(appID, versionID, fileID, action string) *abs.BaseRequestBuilder {
	switch c.appType {
	case pkgType:
		versions := c.beta.MobileApps().ByMobileAppId(appID).GraphMacOSPkgApp().ContentVersions()
		if versionID == "" {
			return &versions.BaseRequestBuilder
		}
		files := versions.ByMobileAppContentId(versionID).Files()
		if fileID == "" {
			return &files.BaseRequestBuilder
		}
		file := files.ByMobileAppContentFileId(fileID)
		switch action {
		case "commit":
			return &file.Commit().BaseRequestBuilder
		case "renewUpload":
			return &file.RenewUpload().BaseRequestBuilder
		default:
			return &file.BaseRequestBuilder
		}
	case dmgType:
		versions := c.beta.MobileApps().ByMobileAppId(appID).GraphMacOSDmgApp().ContentVersions()
		if versionID == "" {
			return &versions.BaseRequestBuilder
		}
		files := versions.ByMobileAppContentId(versionID).Files()
		if fileID == "" {
			return &files.BaseRequestBuilder
		}
		file := files.ByMobileAppContentFileId(fileID)
		switch action {
		case "commit":
			return &file.Commit().BaseRequestBuilder
		case "renewUpload":
			return &file.RenewUpload().BaseRequestBuilder
		default:
			return &file.BaseRequestBuilder
		}
	case lobType:
		versions := c.beta.MobileApps().ByMobileAppId(appID).GraphMacOSLobApp().ContentVersions()
		if versionID == "" {
			return &versions.BaseRequestBuilder
		}
		files := versions.ByMobileAppContentId(versionID).Files()
		if fileID == "" {
			return &files.BaseRequestBuilder
		}
		file := files.ByMobileAppContentFileId(fileID)
		switch action {
		case "commit":
			return &file.Commit().BaseRequestBuilder
		case "renewUpload":
			return &file.RenewUpload().BaseRequestBuilder
		default:
			return &file.BaseRequestBuilder
		}
	case win32Type:
		versions := c.beta.MobileApps().ByMobileAppId(appID).GraphWin32LobApp().ContentVersions()
		if versionID == "" {
			return &versions.BaseRequestBuilder
		}
		files := versions.ByMobileAppContentId(versionID).Files()
		if fileID == "" {
			return &files.BaseRequestBuilder
		}
		file := files.ByMobileAppContentFileId(fileID)
		switch action {
		case "commit":
			return &file.Commit().BaseRequestBuilder
		case "renewUpload":
			return &file.RenewUpload().BaseRequestBuilder
		default:
			return &file.BaseRequestBuilder
		}
	default:
		panic("validated Intune app type is missing")
	}
}

func (c *client) categories() *abs.BaseRequestBuilder {
	return &c.beta.MobileAppCategories().BaseRequestBuilder
}

func (c *client) appCategories(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).Categories().BaseRequestBuilder
}

// Graph's OpenAPI description omits the reference operations that add and
// remove an app's categories. Without a category ID the route adds one.
func (c *client) categoryRef(appID, categoryID string) *abs.BaseRequestBuilder {
	app := c.beta.MobileApps().ByMobileAppId(appID)
	params := maps.Clone(app.PathParameters)
	template := "{+baseurl}/deviceAppManagement/mobileApps/{mobileApp%2Did}/categories/$ref"
	if categoryID != "" {
		params["mobileAppCategory%2Did"] = categoryID
		template = "{+baseurl}/deviceAppManagement/mobileApps/{mobileApp%2Did}/categories/{mobileAppCategory%2Did}/$ref"
	}
	return abs.NewBaseRequestBuilder(app.RequestAdapter, template, params)
}
