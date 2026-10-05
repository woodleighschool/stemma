package intune

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
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

// identifyArtifact derives the publication identity of an artifact. A Win32
// setup tree runs its prepared entry point; an existing envelope keeps its own.
func identifyArtifact(ctx context.Context, artifact plugin.Artifact, appType string) (artifactIdentity, error) {
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
	setup := strings.ReplaceAll(artifact.EntryPoint, `\`, "/")
	if setup != "" && (!fs.ValidPath(setup) || setup == "." || strings.Contains(setup, ":")) {
		return artifactIdentity{}, errors.New("the artifact entry point must be a relative Windows payload path")
	}
	if artifact.Tree {
		if setup == "" {
			return artifactIdentity{}, errors.New("Win32 setup trees require an artifact entry point")
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
			return artifactIdentity{}, errors.New("the artifact entry point conflicts with the existing Intune envelope")
		}
		return artifactIdentity{identity: metadata.PayloadSHA256, setup: metadata.SetupFile, envelope: true, metadata: metadata}, nil
	}
	if setup != "" && setup != artifact.Filename {
		return artifactIdentity{}, errors.New("the artifact entry point must name the single-file artifact")
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
	name := uploadFilename(software, artifact, identity)
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

// upload records the payload's version before sending content. The marker only
// describes active content once Graph activates that version.
func (c *client) upload(ctx context.Context, current object, pending publication, prepared *preparedArtifact) (version string, err error) {
	done := plugin.Stage(ctx, "Publishing Intune content", plugin.Detail(prepared.name))
	defer func() { done(err) }()
	appID := text(current["id"])
	first := text(current["committedContentVersion"]) == ""
	recorded, err := recoverMarker(current, pending.identity)
	if err != nil {
		return "", err
	}
	if first && recorded.payload != pending.payload {
		return "", fmt.Errorf("intune app %s first upload does not match the recorded payload", appID)
	}
	version = pending.content
	resuming := version != ""
	if version == "" && (first || recorded.content == "" && recorded.payload == pending.payload) {
		// The creation marker identifies the first payload before its version
		// is known, including when the activation reply was lost.
		versions, err := c.list(ctx, c.content(appID, "", "", ""))
		if err != nil {
			return "", err
		}
		if len(versions) > 1 {
			return "", fmt.Errorf("intune app %s has multiple content versions without a recorded version", appID)
		}
		if len(versions) == 1 {
			version = text(versions[0]["id"])
			if version == "" {
				return "", errors.New("content version omitted ID")
			}
		}
	}
	if version == "" {
		var created object
		if err := c.request(ctx, abs.POST, c.content(appID, "", "", ""), object{"@odata.type": "#microsoft.graph.mobileAppContent"}, &created); err != nil {
			return "", err
		}
		version = text(created["id"])
		if version == "" {
			return "", errors.New("content version creation omitted ID")
		}
	}
	files, err := c.list(ctx, c.content(appID, version, "", ""))
	if err != nil {
		return "", err
	}
	if len(files) > 1 {
		return "", fmt.Errorf("intune app %s content version %s has multiple files", appID, version)
	}
	var file object
	if len(files) == 1 {
		file = files[0]
		if recorded.payload != pending.payload || text(file["name"]) != prepared.name || file["size"] != float64(prepared.metadata.PlaintextSize) || file["sizeEncrypted"] != float64(prepared.metadata.EncryptedContentSize) {
			return "", fmt.Errorf("intune app %s content version %s does not match the recorded upload", appID, version)
		}
	}
	// Graph refuses every other change to an app before its first publication,
	// so a first upload is found again as the app's only content version.
	if !first && pending.content != version {
		pending.content = version
		if err := c.request(ctx, abs.GET, c.app(appID), nil, &current); err != nil {
			return "", err
		}
		patch := object{"@odata.type": c.appType, "notes": withMarker(noteText(current, nil), pending)}
		if err := c.request(ctx, abs.PATCH, c.app(appID), patch, nil); err != nil {
			return "", err
		}
	}
	if err := c.uploadFile(ctx, appID, version, file, prepared); err != nil {
		if _, rejected := errors.AsType[uploadStateError](err); rejected {
			if first {
				return "", fmt.Errorf("intune app %s cannot finish its first upload; delete the app to publish again: %w", appID, err)
			}
			if resuming {
				pending.content = ""
				return c.upload(ctx, current, pending, prepared)
			}
		}
		return "", err
	}
	return version, nil
}

func (c *client) uploadFile(ctx context.Context, appID, version string, file object, prepared *preparedArtifact) error {
	if file == nil {
		body := object{"@odata.type": "#microsoft.graph.mobileAppContentFile", "name": prepared.name, "size": prepared.metadata.PlaintextSize, "sizeEncrypted": prepared.metadata.EncryptedContentSize, "isDependency": false}
		if err := c.request(ctx, abs.POST, c.content(appID, version, "", ""), body, &file); err != nil {
			return err
		}
	}
	fileID := text(file["id"])
	if fileID == "" {
		return errors.New("content file omitted ID")
	}
	if file["isCommitted"] == true {
		return nil
	}
	fileBuilder := c.content(appID, version, fileID, "")
	if file["uploadState"] == "commitFilePending" {
		_, err := c.waitFile(ctx, fileBuilder, true)
		return err
	}
	file, err := c.waitFile(ctx, fileBuilder, false)
	if err != nil {
		return err
	}
	expiry, _ := time.Parse(time.RFC3339, text(file["azureStorageUriExpirationDateTime"]))
	if !expiry.IsZero() && time.Until(expiry) < 5*time.Minute {
		if err := c.request(ctx, abs.POST, c.content(appID, version, fileID, "renewUpload"), object{}, nil); err != nil {
			return err
		}
		if file, err = c.waitFile(ctx, fileBuilder, false); err != nil {
			return err
		}
	}
	if err := c.uploadBlob(ctx, text(file["azureStorageUri"]), prepared); err != nil {
		return err
	}
	if err := c.request(ctx, abs.POST, c.content(appID, version, fileID, "commit"), object{"fileEncryptionInfo": prepared.metadata.EncryptionInfo}, nil); err != nil {
		return err
	}
	_, err = c.waitFile(ctx, fileBuilder, true)
	return err
}

type uploadStateError string

func (state uploadStateError) Error() string { return "intune upload state " + string(state) }

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
			return nil, uploadStateError(state)
		}
		if err := c.pause(ctx); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("intune file operation exceeded poll limit")
}

func (c *client) uploadBlob(ctx context.Context, sas string, prepared *preparedArtifact) (err error) {
	done := plugin.Stage(ctx, "Uploading Intune content", plugin.Detail(prepared.name))
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
	// Intune commits content only from a committed block list. The SDK's
	// streaming upload sends a payload smaller than one block as a single Put
	// Blob, which leaves the file in commitFileFailed.
	body := plugin.ProgressReader(ctx, reader, prepared.metadata.EncryptedContentSize)
	block := make([]byte, 4<<20)
	var ids []string
	for {
		n, readErr := io.ReadFull(body, block)
		if n > 0 {
			id := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%08d", len(ids)))
			if _, err := blob.StageBlock(ctx, id, streaming.NopCloser(bytes.NewReader(block[:n])), nil); err != nil {
				return azureError(ctx, err)
			}
			ids = append(ids, id)
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if _, err := blob.CommitBlockList(ctx, ids, nil); err != nil {
		return azureError(ctx, err)
	}
	return nil
}

// azureError describes a failed Azure upload request without the SAS URL that
// the SDK's error text includes.
func azureError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if responseErr, ok := errors.AsType[*azcore.ResponseError](err); ok {
		if responseErr.ErrorCode != "" {
			return fmt.Errorf("azure upload HTTP status %d (%s)", responseErr.StatusCode, responseErr.ErrorCode)
		}
		return fmt.Errorf("azure upload HTTP status %d", responseErr.StatusCode)
	}
	return errors.New("azure upload failed; SAS URL omitted from error")
}

func uploadFilename(software string, artifact plugin.Artifact, identity artifactIdentity) string {
	if identity.raw {
		return artifact.Filename
	}
	return artifactname.Filename(software, artifact.Version, artifact.SHA256, "intunewin")
}
