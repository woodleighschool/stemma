package config

import (
	"errors"
	"path/filepath"
	"strings"

	"oras.land/oras-go/v2/registry"
)

func (p Plugin) Validate() error {
	if (p.Image == "") == (p.Path == "") {
		return errors.New("specify exactly one of image or path")
	}
	if p.Image != "" {
		ref, err := registry.ParseReference(p.Image)
		if err != nil || ref.Reference == "" {
			return errors.New("image must be an OCI registry reference with a tag or digest")
		}
		if p.Entrypoint != "" {
			return errors.New("entrypoint requires a local directory path")
		}
	}
	if strings.ContainsAny(p.Path, "\x00\r\n") {
		return errors.New("invalid plugin path")
	}
	if p.Entrypoint != "" && (!filepath.IsLocal(p.Entrypoint) || strings.ContainsAny(p.Entrypoint, "\\:\x00\r\n")) {
		return errors.New("entrypoint must stay inside the plugin directory")
	}
	return nil
}
