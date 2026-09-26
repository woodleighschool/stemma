package config

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/oras-project/oras-go/v3/registry/remote/properties"
)

func (p Plugin) Validate() error {
	if (p.Image == "") == (p.Path == "") {
		return errors.New("specify exactly one of image or path")
	}
	if p.Image != "" {
		// ORAS parses and drops a URL scheme; plugin images carry none.
		ref, err := properties.NewReference(p.Image)
		if err != nil || ref.GetReference() == "" || strings.Contains(p.Image, "://") {
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
