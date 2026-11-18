package intune

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image/png"
	"io"
	"os"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// reconcileIcon returns the icon upload the app needs, if any, and makes it
// when applying.
func (c *client) reconcileIcon(ctx context.Context, req plugin.ReconcileRequest[Config], current object, apply bool) (*plugin.Upload, error) {
	artifact := req.Inputs["icon"]
	if artifact.SHA256 == "" {
		return nil, nil
	}
	existing, _ := current["largeIcon"].(object)
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size || artifact.Tree || artifact.Format != "png" || artifact.Size <= 0 || artifact.Size > 32<<20 {
		return nil, errors.New("intune icon requires a bounded PNG artifact")
	}
	upload := &plugin.Upload{Name: plugin.UploadIcon, Filename: artifact.Filename, After: artifact.SHA256}
	if data, err := base64.StdEncoding.DecodeString(text(existing["value"])); err == nil && len(data) > 0 {
		hash := sha256.Sum256(data)
		upload.Before = hex.EncodeToString(hash[:])
		if text(existing["type"]) == "image/png" && int64(len(data)) == artifact.Size && upload.Before == artifact.SHA256 {
			return nil, nil
		}
	}
	if !apply {
		return upload, nil
	}
	file, err := os.Open(artifact.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: file}, artifact.Size+1))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if int64(len(data)) != artifact.Size || hex.EncodeToString(hash[:]) != artifact.SHA256 {
		return nil, errors.New("intune icon differs from its artifact digest")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 {
		return nil, errors.New("intune icon requires a valid bounded PNG")
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	value := base64.StdEncoding.EncodeToString(data)
	id := text(current["id"])
	if id == "" {
		return nil, errors.New("intune icon requires an existing app")
	}
	icon := object{"@odata.type": "#microsoft.graph.mimeContent", "type": "image/png", "value": value}
	if err := c.request(ctx, abs.PATCH, c.app(id), object{"@odata.type": c.appType, "largeIcon": icon}, nil); err != nil {
		return nil, err
	}
	var readback object
	if err := c.request(ctx, abs.GET, c.app(id), nil, &readback); err != nil {
		return nil, err
	}
	observed, _ := readback["largeIcon"].(object)
	if text(observed["value"]) != value || text(observed["type"]) != "image/png" {
		return nil, errors.New("intune icon readback differs from prepared content")
	}
	return upload, nil
}
