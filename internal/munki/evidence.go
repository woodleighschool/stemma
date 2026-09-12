package munki

import (
	"encoding/json"
	"errors"

	"github.com/woodleighschool/stemma/plugin"
)

// The preparation kind selects an application once; native publishers retain
// responsibility for translating that evidence to their own detection fields.
func macEvidence(artifact plugin.Artifact) (*plugin.Subject, string, error) {
	var app *plugin.Subject
	versionKey := "CFBundleShortVersionString"
	if data, ok := artifact.Evidence["macos.application"]; ok {
		if err := json.Unmarshal(data, &app); err != nil || app == nil || app.App == nil {
			return nil, "", errors.New("macos.application evidence requires an application subject")
		}
	}
	if data, ok := artifact.Evidence["macos.version_key"]; ok {
		if err := json.Unmarshal(data, &versionKey); err != nil || (versionKey != "CFBundleVersion" && versionKey != "CFBundleShortVersionString") {
			return nil, "", errors.New("unsupported macos.version_key evidence")
		}
	}

	return app, versionKey, nil
}
