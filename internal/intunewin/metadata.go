// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

// Package intunewin writes and verifies portable Intune Win32 content envelopes.
// It verifies format integrity, not the publisher or Intune acceptance.
package intunewin

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"github.com/woodleighschool/stemma/internal/intunecontent"
)

const (
	contentName   = "IntunePackage.intunewin"
	contentEntry  = "IntuneWinPackage/Contents/" + contentName
	metadataEntry = "IntuneWinPackage/Metadata/Detection.xml"
	profile       = "ProfileVersion1"
	maxContent    = int64(2 << 30)
	maxMetadata   = int64(1 << 20)
	maxFiles      = 10_000
)

// EncryptionInfo contains the Graph content commit parameters.
type EncryptionInfo = intunecontent.EncryptionInfo

// Metadata identifies the deterministic payload and the actual random envelope.
type Metadata struct {
	Name                 string         `json:"name"`
	SetupFile            string         `json:"setupFile"`
	PayloadSHA256        string         `json:"payloadSHA256"`
	PlaintextSize        int64          `json:"plaintextSize"`
	EncryptedContentSize int64          `json:"encryptedContentSize"`
	EncryptionInfo       EncryptionInfo `json:"encryptionInfo"`
}

type detection struct {
	XMLName        xml.Name       `xml:"ApplicationInfo"`
	XMLNSXSI       string         `xml:"xmlns:xsi,attr,omitempty"`
	XMLNSXSD       string         `xml:"xmlns:xsd,attr,omitempty"`
	ToolVersion    string         `xml:"ToolVersion,attr"`
	Name           string         `xml:"Name"`
	Size           int64          `xml:"UnencryptedContentSize"`
	FileName       string         `xml:"FileName"`
	SetupFile      string         `xml:"SetupFile"`
	EncryptionInfo EncryptionInfo `xml:"EncryptionInfo"`
}

func marshalDetection(m Metadata) ([]byte, error) {
	d := detection{
		XMLNSXSI:    "http://www.w3.org/2001/XMLSchema-instance",
		XMLNSXSD:    "http://www.w3.org/2001/XMLSchema",
		ToolVersion: "1.8.6.0", Name: m.Name, Size: m.PlaintextSize,
		FileName: contentName, SetupFile: m.SetupFile, EncryptionInfo: m.EncryptionInfo,
	}
	data, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), data...), nil
}

func parseDetection(data []byte) (Metadata, error) {
	var d detection
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&d); err != nil {
		return Metadata{}, fmt.Errorf("detection.xml: %w", err)
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Metadata{}, err
		}
		if value, ok := token.(xml.CharData); !ok || len(bytes.TrimSpace(value)) != 0 {
			return Metadata{}, errors.New("trailing content after Detection.xml root")
		}
	}
	if d.ToolVersion == "" || d.Name == "" || d.FileName != contentName || d.Size <= 0 || d.Size > maxContent {
		return Metadata{}, errors.New("invalid Detection.xml content metadata")
	}
	if d.EncryptionInfo.ProfileIdentifier != profile || d.EncryptionInfo.FileDigestAlgorithm != "SHA256" {
		return Metadata{}, errors.New("unsupported intunewin encryption profile or digest algorithm")
	}
	if _, err := payloadPath(d.SetupFile, true); err != nil {
		return Metadata{}, fmt.Errorf("setup file: %w", err)
	}
	return Metadata{Name: d.Name, SetupFile: d.SetupFile, PlaintextSize: d.Size, EncryptionInfo: d.EncryptionInfo}, nil
}
