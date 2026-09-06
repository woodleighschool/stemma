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

// All three app types share the content protocol; the generated SDK builders
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
