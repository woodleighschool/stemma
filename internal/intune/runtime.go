package intune

import "github.com/woodleighschool/stemma/plugin"

// RuntimeRequirements is empty because content preparation is portable Go.
// Microsoft's comparison tool requires Windows and .NET Framework 4.7.2;
// endpoint detection and installation require an enrolled Windows test device.
func RuntimeRequirements() []plugin.Requirement { return nil }

// ContentContract accepts Win32 setup files or trees and native macOS files.
// The metadata subtype applies the narrower format and entrypoint checks.
func ContentContract() *plugin.ContentContract { return &plugin.ContentContract{Trees: true} }

func setupFile(metadata object) string {
	content, _ := metadata["content"].(object)
	return text(content["setup_file"])
}
