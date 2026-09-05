// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

// Package msi inspects the Property table and package code of Windows installers.
// It does not execute installers, inspect transforms, or verify signatures.
package msi

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const maxFileSize = 256 << 20

// Info records installer-declared metadata. Missing optional properties stay
// empty; these claims do not establish publisher identity or install behavior.
type Info struct {
	ProductCode    string            `json:"productCode"`
	ProductVersion string            `json:"productVersion"`
	ProductName    string            `json:"productName"`
	Manufacturer   string            `json:"manufacturer"`
	UpgradeCode    string            `json:"upgradeCode,omitempty"`
	PackageCode    string            `json:"packageCode,omitempty"`
	Properties     map[string]string `json:"properties"`
}

// Read inspects a regular MSI up to 256 MiB. The supported subset uses compound
// files with 512/4096-byte sectors and ASCII, Windows-1252, UTF-8 or UTF-16 strings.
// Invalid structures and unsupported encodings return errors, never guessed facts.
func Read(path string) (Info, error) {
	file, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		return Info{}, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxFileSize {
		return Info{}, errors.New("MSI must be a regular file no larger than 256 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return Info{}, err
	}
	if len(data) > maxFileSize {
		return Info{}, errors.New("MSI exceeds 256 MiB")
	}
	db, err := newDatabase(data)
	if err != nil {
		return Info{}, fmt.Errorf("MSI compound file: %w", err)
	}
	props, err := db.readProperties()
	if err != nil {
		return Info{}, fmt.Errorf("MSI Property table: %w", err)
	}
	if props["ProductCode"] == "" || props["ProductName"] == "" || props["ProductVersion"] == "" {
		return Info{}, errors.New("missing required MSI product properties")
	}
	return Info{
		ProductCode: props["ProductCode"], ProductVersion: props["ProductVersion"], ProductName: props["ProductName"],
		Manufacturer: props["Manufacturer"], UpgradeCode: props["UpgradeCode"], PackageCode: db.packageCode(), Properties: props,
	}, nil
}
