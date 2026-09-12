package munki

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/woodleighschool/stemma/plugin"
)

// The preparation kind selects an application once; native publishers retain
// responsibility for translating that evidence to their own detection fields.
func macEvidence(artifact plugin.Artifact) (*plugin.Subject, string, []string, error) {
	var app *plugin.Subject
	versionKey := "CFBundleShortVersionString"
	var architectures []string
	if data, ok := artifact.Evidence["macos.application"]; ok {
		if err := json.Unmarshal(data, &app); err != nil || app == nil || app.App == nil {
			return nil, "", nil, errors.New("macos.application evidence requires an application subject")
		}
	}
	if data, ok := artifact.Evidence["macos.version_key"]; ok {
		if err := json.Unmarshal(data, &versionKey); err != nil || (versionKey != "CFBundleVersion" && versionKey != "CFBundleShortVersionString") {
			return nil, "", nil, errors.New("unsupported macos.version_key evidence")
		}
	}
	if data, ok := artifact.Evidence["macos.arch"]; ok {
		var architecture string
		if err := json.Unmarshal(data, &architecture); err != nil {
			return nil, "", nil, fmt.Errorf("macos.arch evidence: %w", err)
		}
		switch architecture {
		case "arm64", "x86_64":
			architectures = []string{architecture}
		case "universal":
			architectures = []string{"arm64", "x86_64"}
		default:
			return nil, "", nil, fmt.Errorf("unsupported macos.arch evidence %q", architecture)
		}
	}
	return app, versionKey, architectures, nil
}
