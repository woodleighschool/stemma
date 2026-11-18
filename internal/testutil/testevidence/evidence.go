// Package testevidence reads and writes the host's evidence namespaces in tests.
package testevidence

import "encoding/json"

// field returns one member of an evidence namespace, or nil when it is absent.
func field(evidence map[string]json.RawMessage, namespace, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(evidence[namespace], &fields) != nil {
		return nil
	}
	return fields[name]
}

// Application is the selected macOS application subject.
func Application(evidence map[string]json.RawMessage) json.RawMessage {
	return field(evidence, "macos", "application")
}

// VersionKey is the macOS application's version key.
func VersionKey(evidence map[string]json.RawMessage) json.RawMessage {
	return field(evidence, "macos", "version_key")
}

// Installer is the selected Windows MSI subject.
func Installer(evidence map[string]json.RawMessage) json.RawMessage {
	return field(evidence, "windows", "installer")
}

// Signatures are the signing observations.
func Signatures(evidence map[string]json.RawMessage) json.RawMessage {
	return field(evidence, "signing", "observed")
}

// Unverified are the signing subjects taken in without an expectation.
func Unverified(evidence map[string]json.RawMessage) json.RawMessage {
	return field(evidence, "signing", "unverified")
}

// Build is the signing state a builder gave its output.
func Build(evidence map[string]json.RawMessage) json.RawMessage {
	return field(evidence, "signing", "build")
}

// MacOS is a macos namespace selecting an application subject.
func MacOS(application json.RawMessage) json.RawMessage {
	return json.RawMessage(`{"application":` + string(application) + `}`)
}

// Windows is a windows namespace selecting an MSI subject.
func Windows(installer json.RawMessage) json.RawMessage {
	return json.RawMessage(`{"installer":` + string(installer) + `}`)
}
