package jamf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/png"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	sdkicon "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/icon"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// A selfServiceIcon is the software's icon as policies show it in Self Service.
// Jamf re-encodes uploaded images, so the file name carries the source digest
// and identifies the icon a policy shows.
type selfServiceIcon struct {
	artifact plugin.Artifact
	// id is an existing or newly uploaded icon, shared by policies this run.
	id string
}

func newSelfServiceIcon(artifact plugin.Artifact) (*selfServiceIcon, error) {
	if artifact.Path == "" {
		return nil, nil
	}
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size || artifact.Tree || artifact.Format != "png" || artifact.Size <= 0 || artifact.Size > 32<<20 {
		return nil, errors.New("jamf icon requires a bounded PNG artifact")
	}
	return &selfServiceIcon{artifact: artifact}, nil
}

func (i *selfServiceIcon) filename() string {
	return strings.ToLower(i.artifact.SHA256) + ".png"
}

// observe reuses the uploaded icon a policy already shows. Planning observes
// every policy before applying, so declaration order cannot cause another upload.
func (i *selfServiceIcon) observe(doc *xmlNode, path ...string) {
	if i == nil || i.id != "" {
		return
	}
	for _, name := range path {
		doc = doc.child(name)
	}
	icon := doc.child("self_service_icon")
	if icon.value("filename") == i.filename() && validID(icon.value("id")) {
		i.id = icon.value("id")
	}
}

// setting shows the icon at self_service_icon under path, uploading it when
// no reusable ID has been observed.
func (i *selfServiceIcon) setting(c *client, root string, path ...string) setting {
	path = slices.Concat(path, []string{"self_service_icon"})
	filename := slices.Concat(path, []string{"filename"})
	return setting{
		field:    "self_service.icon",
		value:    i.filename(),
		fragment: fragment(root, i.filename(), filename...),
		current:  text(filename...),
		write: func(ctx context.Context) (*xmlNode, error) {
			if i.id == "" {
				id, err := c.uploadIcon(ctx, i)
				if err != nil {
					return nil, err
				}
				i.id = id
			}
			return whole(fragment(root, map[string]string{"id": i.id}, path...), path...), nil
		},
	}
}

func (c *client) uploadIcon(ctx context.Context, icon *selfServiceIcon) (id string, err error) {
	done := plugin.Stage(ctx, "Uploading Jamf icon")
	defer func() { done(err) }()
	f, err := os.Open(icon.artifact.Path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: f}, icon.artifact.Size+1))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != icon.artifact.Size || hex.EncodeToString(digest[:]) != strings.ToLower(icon.artifact.SHA256) {
		return "", errors.New("jamf icon differs from its artifact digest")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return "", errors.New("jamf icon requires a valid bounded PNG")
	}
	result, response, err := sdkicon.NewIcon(c.transport).UploadV1(ctx, bytes.NewReader(data), int64(len(data)), icon.filename())
	if err := requestError(ctx, response, err); err != nil {
		return "", err
	}
	if result == nil || result.ID <= 0 {
		return "", errors.New("jamf icon upload returned no valid ID")
	}
	return strconv.Itoa(result.ID), nil
}
