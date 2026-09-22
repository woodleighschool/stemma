package intune

import abs "github.com/microsoft/kiota-abstractions-go"

func (c *client) apps() *abs.BaseRequestBuilder {
	if c.appType != win32Type {
		return &c.beta.MobileApps().BaseRequestBuilder
	}
	return &c.stable.MobileApps().BaseRequestBuilder
}
func (c *client) app(id string) *abs.BaseRequestBuilder {
	if c.appType != win32Type {
		return &c.beta.MobileApps().ByMobileAppId(id).BaseRequestBuilder
	}
	return &c.stable.MobileApps().ByMobileAppId(id).BaseRequestBuilder
}
func (c *client) assignments(id string) *abs.BaseRequestBuilder {
	if c.appType != win32Type {
		return &c.beta.MobileApps().ByMobileAppId(id).Assignments().BaseRequestBuilder
	}
	return &c.stable.MobileApps().ByMobileAppId(id).Assignments().BaseRequestBuilder
}
func (c *client) assign(id string) *abs.BaseRequestBuilder {
	if c.appType != win32Type {
		return &c.beta.MobileApps().ByMobileAppId(id).Assign().BaseRequestBuilder
	}
	return &c.stable.MobileApps().ByMobileAppId(id).Assign().BaseRequestBuilder
}

func (c *client) relationships(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).Relationships().BaseRequestBuilder
}

func (c *client) updateRelationships(id string) *abs.BaseRequestBuilder {
	return &c.beta.MobileApps().ByMobileAppId(id).UpdateRelationships().BaseRequestBuilder
}

func (c *client) contentVersion(appID, versionID string) *abs.BaseRequestBuilder {
	switch c.appType {
	case pkgType:
		return &c.beta.MobileApps().ByMobileAppId(appID).GraphMacOSPkgApp().ContentVersions().ByMobileAppContentId(versionID).BaseRequestBuilder
	case dmgType:
		return &c.beta.MobileApps().ByMobileAppId(appID).GraphMacOSDmgApp().ContentVersions().ByMobileAppContentId(versionID).BaseRequestBuilder
	case lobType:
		return &c.beta.MobileApps().ByMobileAppId(appID).GraphMacOSLobApp().ContentVersions().ByMobileAppContentId(versionID).BaseRequestBuilder
	default:
		return &c.stable.MobileApps().ByMobileAppId(appID).GraphWin32LobApp().ContentVersions().ByMobileAppContentId(versionID).BaseRequestBuilder
	}
}

// Every app type shares the content protocol; the generated SDK builders
// select their supported endpoint, including beta for current macOS requirements.
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
		versions := c.stable.MobileApps().ByMobileAppId(appID).GraphWin32LobApp().ContentVersions()
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
