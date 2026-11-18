package munki

import "github.com/woodleighschool/stemma/plugin"

// The preparation kind selects an application once; native publishers retain
// responsibility for translating that evidence to their own detection fields.
func macEvidence(artifact plugin.Artifact) (*plugin.Subject, string, error) {
	macos, err := plugin.ReadMacOS(artifact.Evidence)
	return macos.Application, macos.VersionKey, err
}
