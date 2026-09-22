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

func (c *client) reconcileIcon(ctx context.Context, req plugin.ReconcileRequest[Config], current object, apply bool) ([]plugin.Change, error) {
	artifact := req.Inputs["icon"]
	if artifact.Path == "" {
		return nil, nil
	}
	existing, _ := current["largeIcon"].(object)
	if artifact.Tree || artifact.Format != "png" || artifact.Size <= 0 || artifact.Size > 32<<20 {
		return nil, errors.New("intune icon requires a bounded PNG artifact")
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
	digest := sha256.Sum256(data)
	if int64(len(data)) != artifact.Size || hex.EncodeToString(digest[:]) != artifact.SHA256 {
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
	if text(existing["value"]) == value && text(existing["type"]) == "image/png" {
		return nil, nil
	}
	changes := []plugin.Change{{Kind: "content", Field: "icon", Action: "upload", After: raw(artifact.SHA256)}}
	if !apply {
		return changes, nil
	}
	id := text(current["id"])
	if id == "" {
		return changes, errors.New("intune icon requires an existing app")
	}
	icon := object{"@odata.type": "#microsoft.graph.mimeContent", "type": "image/png", "value": value}
	if err := c.request(ctx, abs.PATCH, c.app(id), object{"@odata.type": c.appType, "largeIcon": icon}, nil); err != nil {
		return changes, err
	}
	var readback object
	if err := c.request(ctx, abs.GET, c.app(id), nil, &readback); err != nil {
		return changes, err
	}
	observed, _ := readback["largeIcon"].(object)
	if text(observed["value"]) != value || text(observed["type"]) != "image/png" {
		return changes, errors.New("intune icon readback differs from prepared content")
	}
	return changes, nil
}
