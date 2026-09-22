package intune

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/artifactname"
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
	path     string
	name     string
	metadata intunecontent.Info
	setup    string
	raw      bool
	cleanup  func()
}

func (p *preparedArtifact) close() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

func identifyArtifact(ctx context.Context, artifact plugin.Artifact, appType, setup string) (artifactIdentity, error) {
	if artifact.Path == "" || (!artifact.Tree && (artifact.Filename == "" || filepath.Base(artifact.Filename) != artifact.Filename || strings.ContainsAny(artifact.Filename, `\:`))) {
		return artifactIdentity{}, errors.New("intune requires immutable content; file artifacts need a simple filename")
	}
	digest, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return artifactIdentity{}, errors.New("intune artifact requires a SHA-256 digest")
	}
	if appType != win32Type {
		if artifact.Tree {
			return artifactIdentity{}, errors.New("macOS Intune apps require a file artifact")
		}
		extension := ".dmg"
		if appType == pkgType || appType == lobType {
			extension = ".pkg"
		}
		if !strings.EqualFold(filepath.Ext(artifact.Filename), extension) {
			return artifactIdentity{}, fmt.Errorf("%s requires an unwrapped %s source", appType, extension)
		}
		return artifactIdentity{identity: artifact.SHA256, raw: true}, nil
	}
	setup = strings.ReplaceAll(setup, `\`, "/")
	entrypoint := strings.ReplaceAll(artifact.EntryPoint, `\`, "/")
	if setup != "" && entrypoint != "" && setup != entrypoint {
		return artifactIdentity{}, errors.New("content.setup_file conflicts with the artifact entrypoint")
	}
	if setup == "" {
		setup = entrypoint
	}
	if setup != "" && (!fs.ValidPath(setup) || setup == "." || strings.Contains(setup, ":")) {
		return artifactIdentity{}, errors.New("content.setup_file must be a relative Windows payload path")
	}
	if artifact.Tree {
		if setup == "" {
			return artifactIdentity{}, errors.New("Win32 setup trees require content.setup_file or an artifact entrypoint")
		}
		if err := intunewin.ValidateSource(ctx, artifact.Path, setup); err != nil {
			return artifactIdentity{}, fmt.Errorf("intune setup tree: %w", err)
		}
		hash := sha256.Sum256([]byte("stemma-intune-tree-v1\x00" + artifact.SHA256 + "\x00" + setup))
		return artifactIdentity{identity: hex.EncodeToString(hash[:]), setup: strings.ReplaceAll(setup, "/", `\`)}, nil
	}
	if strings.EqualFold(filepath.Ext(artifact.Filename), ".intunewin") {
		metadata, err := intunewin.Inspect(ctx, artifact.Path)
		if err != nil {
			return artifactIdentity{}, err
		}
		if setup != "" && setup != strings.ReplaceAll(metadata.SetupFile, `\`, "/") {
			return artifactIdentity{}, errors.New("content.setup_file conflicts with the existing Intune envelope")
		}
		return artifactIdentity{identity: metadata.PayloadSHA256, setup: metadata.SetupFile, envelope: true, metadata: metadata}, nil
	}
	if setup != "" && setup != artifact.Filename {
		return artifactIdentity{}, errors.New("content.setup_file must name the single-file artifact")
	}
	// The setup name is part of the one-file derivation; renaming identical bytes
	// must not leave Graph pointing at a name absent from the uploaded payload.
	hash := sha256.Sum256([]byte("stemma-intune-file-v1\x00" + artifact.SHA256 + "\x00" + artifact.Filename))
	return artifactIdentity{identity: hex.EncodeToString(hash[:]), setup: artifact.Filename}, nil
}

func contentInfo(m intunewin.Metadata) intunecontent.Info {
	return intunecontent.Info{PayloadSHA256: m.PayloadSHA256, PlaintextSize: m.PlaintextSize, EncryptedContentSize: m.EncryptedContentSize, EncryptionInfo: m.EncryptionInfo}
}

func prepareArtifact(ctx context.Context, software string, artifact plugin.Artifact, identity artifactIdentity) (*preparedArtifact, error) {
	name := artifact.Filename
	if !identity.raw {
		name = artifactname.Filename(software, artifact.Version, artifact.SHA256, "intunewin")
	}
	if identity.envelope {
		return &preparedArtifact{path: artifact.Path, name: name, metadata: contentInfo(identity.metadata), setup: identity.setup}, nil
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
	if artifact.Tree {
		if err := snapshotTree(ctx, artifact, workspace, source); err != nil {
			return nil, err
		}
	} else if err := snapshotFile(ctx, artifact, source); err != nil {
		return nil, err
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
		m, err := intunewin.Write(ctx, source, strings.ReplaceAll(identity.setup, `\`, "/"), path)
		if err != nil {
			return nil, err
		}
		metadata = contentInfo(m)
	}
	success = true
	return &preparedArtifact{path: path, name: name, metadata: metadata, setup: identity.setup, raw: identity.raw, cleanup: func() { _ = os.RemoveAll(workspace) }}, nil
}

func snapshotTree(ctx context.Context, artifact plugin.Artifact, workspace, source string) error {
	snapshot, err := os.Create(filepath.Join(workspace, "source.tar"))
	if err != nil {
		return err
	}
	hash := sha256.New()
	err = archive.Pack(ctx, artifact.Path, io.MultiWriter(snapshot, hash))
	info, statErr := snapshot.Stat()
	err = errors.Join(err, statErr, snapshot.Close())
	if err != nil {
		return err
	}
	if info.Size() != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return errors.New("intune setup tree changed from its immutable digest")
	}
	return archive.Extract(ctx, snapshot.Name(), source)
}

func snapshotFile(ctx context.Context, artifact plugin.Artifact, source string) error {
	if err := os.Mkdir(source, 0o700); err != nil {
		return err
	}
	input, err := os.Open(artifact.Path)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	stat, err := input.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 2<<30 {
		return errors.New("intune input must be a regular file no larger than 2 GiB")
	}
	output, err := os.Create(filepath.Join(source, artifact.Filename))
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(fileio.Reader{Context: ctx, Reader: input}, 2<<30+1))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > 2<<30 || n != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return errors.New("intune source changed from its immutable digest")
	}
	return nil
}

// upload commits a new content version; the caller activates it.
func (c *client) upload(ctx context.Context, appID string, prepared *preparedArtifact) (version string, err error) {
	done := plugin.Stage(ctx, "Publishing Intune content")
	defer func() { done(err) }()
	var created object
	if err := c.request(ctx, abs.POST, c.content(appID, "", "", ""), object{"@odata.type": "#microsoft.graph.mobileAppContent"}, &created); err != nil {
		return "", err
	}
	version = text(created["id"])
	if version == "" {
		return "", errors.New("content version creation omitted ID")
	}
	var file object
	body := object{"@odata.type": "#microsoft.graph.mobileAppContentFile", "name": prepared.name, "size": prepared.metadata.PlaintextSize, "sizeEncrypted": prepared.metadata.EncryptedContentSize, "isDependency": false}
	if err := c.request(ctx, abs.POST, c.content(appID, version, "", ""), body, &file); err != nil {
		return "", err
	}
	fileID := text(file["id"])
	if fileID == "" {
		return "", errors.New("content file creation omitted ID")
	}
	fileBuilder := c.content(appID, version, fileID, "")
	if file, err = c.waitFile(ctx, fileBuilder, false); err != nil {
		return "", err
	}
	expiry, _ := time.Parse(time.RFC3339, text(file["azureStorageUriExpirationDateTime"]))
	if !expiry.IsZero() && time.Until(expiry) < 5*time.Minute {
		if err := c.request(ctx, abs.POST, c.content(appID, version, fileID, "renewUpload"), object{}, nil); err != nil {
			return "", err
		}
		if file, err = c.waitFile(ctx, fileBuilder, false); err != nil {
			return "", err
		}
	}
	if err := c.uploadBlob(ctx, text(file["azureStorageUri"]), prepared); err != nil {
		return "", err
	}
	if err := c.request(ctx, abs.POST, c.content(appID, version, fileID, "commit"), object{"fileEncryptionInfo": prepared.metadata.EncryptionInfo}, nil); err != nil {
		return "", err
	}
	if _, err := c.waitFile(ctx, fileBuilder, true); err != nil {
		return "", err
	}
	return version, nil
}

func (c *client) waitFile(ctx context.Context, builder *abs.BaseRequestBuilder, committed bool) (result object, err error) {
	done := plugin.Stage(ctx, "Waiting for Intune processing")
	defer func() { done(err) }()
	for range 360 {
		var file object
		if err := c.request(ctx, abs.GET, builder, nil, &file); err != nil {
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

func (c *client) uploadBlob(ctx context.Context, sas string, prepared *preparedArtifact) (err error) {
	done := plugin.Stage(ctx, "Uploading Intune content")
	defer func() { done(err) }()
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
	blob, err := blockblob.NewClientWithNoCredential(sas, &blockblob.ClientOptions{Transport: c.http})
	if err != nil {
		return errors.New("cannot configure Azure upload")
	}
	_, err = blob.UploadStream(ctx, plugin.ProgressReader(ctx, reader, prepared.metadata.EncryptedContentSize), &blockblob.UploadStreamOptions{BlockSize: 4 << 20, Concurrency: 1})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if responseErr, ok := errors.AsType[*azcore.ResponseError](err); ok {
			return fmt.Errorf("azure upload HTTP status %d", responseErr.StatusCode)
		}
		return errors.New("azure upload failed; SAS URL omitted from error")
	}
	return nil
}
