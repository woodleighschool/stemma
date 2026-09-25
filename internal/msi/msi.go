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

// Read inspects a regular MSI. The supported subset uses compound files with
// 512/4096-byte sectors and ASCII, Windows-1252, UTF-8 or UTF-16 strings.
// Invalid structures and unsupported encodings return errors, never guessed facts.
func Read(path string) (Info, error) {
	db, file, err := open(path)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = file.Close() }()
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

// ProductIcon returns the icon the installer registers for Programs and
// Features, an ICO or executable stored under the Icon table, and false when
// the installer declares none.
func ProductIcon(path string) ([]byte, bool, error) {
	db, file, err := open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	props, err := db.readProperties()
	if err != nil {
		return nil, false, fmt.Errorf("MSI Property table: %w", err)
	}
	name := props["ARPPRODUCTICON"]
	if name == "" {
		return nil, false, nil
	}
	data, ok, err := db.decoded("Icon." + name)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, fmt.Errorf("MSI Icon table has no %q stream", name)
	}
	return data, true, nil
}

// open reads the database structure of the MSI at path. Streams are read on
// demand, so the caller closes the file once it has read them.
func open(path string) (*database, io.Closer, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	stat, err := file.Stat()
	if err == nil && !stat.Mode().IsRegular() {
		err = errors.New("MSI must be a regular file")
	}
	var db *database
	if err == nil {
		db, err = newDatabase(file, stat.Size())
		if err != nil {
			err = fmt.Errorf("MSI compound file: %w", err)
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return db, file, nil
}
