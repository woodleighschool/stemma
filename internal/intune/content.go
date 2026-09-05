package intune

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/intunecontent"
	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/plugin"
)

type artifactIdentity struct {
	identity string
	setup    string
	envelope bool
	raw      bool
	metadata intunewin.Metadata
}
type preparedArtifact struct {
	path           string
	name           string
	envelopeSHA256 string
	metadata       intunecontent.Info
	setup          string
	raw            bool
	cleanup        func()
}

func (p *preparedArtifact) close() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

func identifyArtifact(artifact plugin.Artifact, appType string) (artifactIdentity, error) {
	if artifact.Path == "" || artifact.Filename == "" || filepath.Base(artifact.Filename) != artifact.Filename || strings.ContainsAny(artifact.Filename, `\:`) {
		return artifactIdentity{}, errors.New("intune requires an immutable file artifact with a simple filename")
	}
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return artifactIdentity{}, errors.New("intune artifact requires a SHA-256 digest")
	}
	if appType != win32Type {
		extension := ".dmg"
		if appType == pkgType {
			extension = ".pkg"
		}
		if !strings.EqualFold(filepath.Ext(artifact.Filename), extension) {
			return artifactIdentity{}, fmt.Errorf("%s requires an unwrapped %s source", appType, extension)
		}
		return artifactIdentity{identity: artifact.SHA256, raw: true}, nil
	}
	if strings.EqualFold(filepath.Ext(artifact.Filename), ".intunewin") {
		metadata, err := intunewin.Inspect(artifact.Path)
		if err != nil {
			return artifactIdentity{}, err
		}
		return artifactIdentity{identity: metadata.PayloadSHA256, setup: metadata.SetupFile, envelope: true, metadata: metadata}, nil
	}
	// The setup name is part of the one-file derivation; renaming identical bytes
	// must not leave Graph pointing at a name absent from the uploaded payload.
	hash := sha256.Sum256([]byte("stemma-intune-file-v1\x00" + artifact.SHA256 + "\x00" + artifact.Filename))
	return artifactIdentity{identity: hex.EncodeToString(hash[:]), setup: artifact.Filename}, nil
}

func contentInfo(m intunewin.Metadata) intunecontent.Info {
	return intunecontent.Info{PayloadSHA256: m.PayloadSHA256, PlaintextSize: m.PlaintextSize, EncryptedContentSize: m.EncryptedContentSize, EncryptionInfo: m.EncryptionInfo}
}

func prepareArtifact(ctx context.Context, artifact plugin.Artifact, identity artifactIdentity) (*preparedArtifact, error) {
	if identity.envelope {
		return &preparedArtifact{path: artifact.Path, name: artifact.Filename, envelopeSHA256: artifact.SHA256, metadata: contentInfo(identity.metadata), setup: identity.setup}, nil
	}
	workspace, err := os.MkdirTemp("", "stemma-intune-")
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(workspace)
		}
	}()
	source := filepath.Join(workspace, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		return nil, err
	}
	input, err := os.Open(artifact.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = input.Close() }()
	stat, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 2<<30 {
		return nil, errors.New("intune input must be a regular file no larger than 2 GiB")
	}
	output, err := os.Create(filepath.Join(source, artifact.Filename))
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(fileio.Reader{Context: ctx, Reader: input}, 2<<30+1))
	closeErr := output.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if n > 2<<30 || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return nil, errors.New("intune source changed from its immutable digest")
	}
	name := artifact.Filename
	if !identity.raw {
		name = strings.TrimSuffix(artifact.Filename, filepath.Ext(artifact.Filename)) + ".intunewin"
	}
	path := filepath.Join(workspace, name)
	var metadata intunecontent.Info
	if identity.raw {
		input, err := os.Open(filepath.Join(source, artifact.Filename))
		if err != nil {
			return nil, err
		}
		defer func() { _ = input.Close() }()
		encrypted, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		metadata, err = intunecontent.Encrypt(ctx, input, encrypted)
		closeErr := encrypted.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else {
		m, err := intunewin.Write(ctx, source, artifact.Filename, path)
		if err != nil {
			return nil, err
		}
		metadata = contentInfo(m)
	}
	envelope, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hash.Reset()
	_, copyErr = io.Copy(hash, fileio.Reader{Context: ctx, Reader: envelope})
	closeErr = envelope.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	success = true
	return &preparedArtifact{path: path, name: name, envelopeSHA256: hex.EncodeToString(hash.Sum(nil)), metadata: metadata, setup: identity.setup, raw: identity.raw, cleanup: func() { _ = os.RemoveAll(workspace) }}, nil
}

func (c *client) contentPath(appID, versionID string) string {
	path := appPath(appID) + "/" + strings.TrimPrefix(c.appType, "#") + "/contentVersions"
	if versionID != "" {
		path += "/" + url.PathEscape(versionID)
	}
	return path
}

func (c *client) upload(ctx context.Context, appID, identity string, prepared *preparedArtifact, b *binding) error {
	if b.Pending == nil {
		b.Pending = &pendingUpload{PayloadSHA256: identity, Stage: "version-request"}
		var created object
		if err := c.request(ctx, http.MethodPost, c.contentPath(appID, ""), object{"@odata.type": "#microsoft.graph.mobileAppContent"}, &created); err != nil {
			return err
		}
		b.Pending.VersionID = text(created["id"])
		if b.Pending.VersionID == "" {
			return errors.New("content version creation omitted ID; outcome is uncertain")
		}
		b.Pending.Stage = "version"
	}
	p := b.Pending
	if p.Stage == "version-request" {
		return errors.New("prior content version creation is uncertain; reconcile the remote version before retrying")
	}
	filesPath := c.contentPath(appID, p.VersionID) + "/files"
	if p.Stage == "version" {
		if prepared == nil {
			return errors.New("prepared content is unavailable")
		}
		p.Name = prepared.name
		p.PlaintextSize = prepared.metadata.PlaintextSize
		p.EncryptedSize = prepared.metadata.EncryptedContentSize
		p.Stage = "file-request"
		var file object
		body := object{"@odata.type": "#microsoft.graph.mobileAppContentFile", "name": p.Name, "size": p.PlaintextSize, "sizeEncrypted": p.EncryptedSize, "isDependency": false}
		if err := c.request(ctx, http.MethodPost, filesPath, body, &file); err != nil {
			return err
		}
		p.FileID = text(file["id"])
		if p.FileID == "" {
			return errors.New("content file creation omitted ID; outcome is uncertain")
		}
		p.Stage = "file"
	}
	if p.Stage == "file-request" {
		files, err := c.list(ctx, filesPath)
		if err != nil {
			return err
		}
		for _, file := range files {
			if text(file["name"]) != p.Name || file["size"] != float64(p.PlaintextSize) || file["sizeEncrypted"] != float64(p.EncryptedSize) {
				continue
			}
			if p.FileID != "" {
				return errors.New("ambiguous content files after interrupted creation")
			}
			p.FileID = text(file["id"])
		}
		if p.FileID == "" {
			return errors.New("prior content file creation is uncertain; refusing blind recreation")
		}
		p.Stage = "file"
	}
	filePath := filesPath + "/" + url.PathEscape(p.FileID)
	if p.Stage == "file" {
		if prepared == nil {
			return errors.New("prepared content is unavailable for upload resume")
		}
		if prepared.metadata.PlaintextSize != p.PlaintextSize || prepared.metadata.EncryptedContentSize != p.EncryptedSize {
			return errors.New("pending Intune upload size differs from reconstructed envelope")
		}
		file, err := c.waitFile(ctx, filePath, false)
		if err != nil {
			return err
		}
		expiry, _ := time.Parse(time.RFC3339, text(file["azureStorageUriExpirationDateTime"]))
		if !expiry.IsZero() && time.Until(expiry) < 5*time.Minute {
			if err := c.request(ctx, http.MethodPost, filePath+"/renewUpload", object{}, nil); err != nil {
				return err
			}
			file, err = c.waitFile(ctx, filePath, false)
			if err != nil {
				return err
			}
		}
		if err := c.uploadBlob(ctx, text(file["azureStorageUri"]), prepared); err != nil {
			return err
		}
		p.EncryptionInfo = prepared.metadata.EncryptionInfo
		p.EnvelopeSHA256 = prepared.envelopeSHA256
		p.Stage = "uploaded"
	}
	if p.Stage == "uploaded" || p.Stage == "committing" {
		if p.Stage == "committing" {
			var file object
			if err := c.request(ctx, http.MethodGet, filePath, nil, &file); err != nil {
				return err
			}
			if file["uploadState"] == "commitFileSuccess" && file["isCommitted"] == true {
				p.Stage = "committed"
				return nil
			}
			if file["uploadState"] == "commitFilePending" {
				if _, err := c.waitFile(ctx, filePath, true); err != nil {
					return err
				}
				p.Stage = "committed"
				return nil
			}
		}
		p.Stage = "committing"
		if err := c.request(ctx, http.MethodPost, filePath+"/commit", object{"fileEncryptionInfo": p.EncryptionInfo}, nil); err != nil {
			return err
		}
		if _, err := c.waitFile(ctx, filePath, true); err != nil {
			return err
		}
		p.Stage = "committed"
	}
	if p.Stage != "committed" {
		return fmt.Errorf("unsupported pending Intune upload stage %q", p.Stage)
	}
	return nil
}

func (c *client) waitFile(ctx context.Context, path string, committed bool) (object, error) {
	for range 360 {
		var file object
		if err := c.request(ctx, http.MethodGet, path, nil, &file); err != nil {
			return nil, err
		}
		state := text(file["uploadState"])
		if committed && state == "commitFileSuccess" && file["isCommitted"] == true {
			return file, nil
		}
		if !committed && (state == "azureStorageUriRequestSuccess" || state == "azureStorageUriRenewalSuccess") && text(file["azureStorageUri"]) != "" {
			return file, nil
		}
		if state == "error" || strings.HasSuffix(state, "Failed") || strings.HasSuffix(state, "TimedOut") {
			return nil, fmt.Errorf("intune upload state %s", state)
		}
		if err := c.pause(ctx); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("intune file operation exceeded poll limit")
}

func (c *client) uploadBlob(ctx context.Context, sas string, prepared *preparedArtifact) error {
	endpoint, err := url.Parse(sas)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && (endpoint.Scheme != "http" || (endpoint.Hostname() != "localhost" && endpoint.Hostname() != "127.0.0.1"))) {
		return errors.New("invalid Azure upload endpoint")
	}
	var reader io.ReadCloser
	if prepared.raw {
		reader, err = os.Open(prepared.path)
	} else {
		envelope, openErr := zip.OpenReader(prepared.path)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = envelope.Close() }()
		var content *zip.File
		for _, entry := range envelope.File {
			if entry.Name == "IntuneWinPackage/Contents/IntunePackage.intunewin" {
				content = entry
			}
		}
		if content == nil {
			return errors.New("encrypted content entry is missing")
		}
		reader, err = content.Open()
	}
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	buffer := make([]byte, 4<<20)
	var blocks []string
	for index := 0; ; index++ {
		n, readErr := io.ReadFull(reader, buffer)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if n > 0 {
			block := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%08d", index))
			query := endpoint.Query()
			query.Set("comp", "block")
			query.Set("blockid", block)
			endpoint.RawQuery = query.Encode()
			if err := c.putBlob(ctx, endpoint.String(), buffer[:n], "application/octet-stream"); err != nil {
				return err
			}
			blocks = append(blocks, block)
		}
		if readErr != nil {
			break
		}
	}
	list := struct {
		XMLName xml.Name `xml:"BlockList"`
		Latest  []string `xml:"Latest"`
	}{Latest: blocks}
	data, err := xml.Marshal(list)
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Del("blockid")
	query.Set("comp", "blocklist")
	endpoint.RawQuery = query.Encode()
	return c.putBlob(ctx, endpoint.String(), data, "application/xml")
}

func (c *client) putBlob(ctx context.Context, endpoint string, data []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("cannot construct Azure upload request")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-ms-version", "2023-11-03")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("azure block upload failed; SAS URL omitted from error")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("azure block upload HTTP status %d", response.StatusCode)
	}
	return nil
}
