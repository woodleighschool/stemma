package munki

import (
	"fmt"

	"howett.net/plist"
)

// Marshal encodes a native Munki repository document as an XML property list.
// This is the format consumed by Munki 7's Foundation PropertyListSerialization.
func Marshal(value any) ([]byte, error) {
	encoded, err := plist.MarshalIndent(value, plist.XMLFormat, "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Munki document: %w", err)
	}
	return encoded, nil
}

// Unmarshal reads a native property-list pkginfo, catalog or manifest. It retains
// unknown fields so shared-repository publication can preserve unowned entries.
func Unmarshal(data []byte, value any) error {
	if _, err := plist.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode Munki document: %w", err)
	}
	return nil
}
