// Copyright 2018-2022 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package upload

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/adler32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	"github.com/cs3org/reva/v2/pkg/appctx"
	ctxpkg "github.com/cs3org/reva/v2/pkg/ctx"
	"github.com/cs3org/reva/v2/pkg/errtypes"
	"github.com/cs3org/reva/v2/pkg/events"
	"github.com/cs3org/reva/v2/pkg/storage/utils/decomposedfs/lookup"
	"github.com/cs3org/reva/v2/pkg/storage/utils/decomposedfs/metadata/prefixes"
	"github.com/cs3org/reva/v2/pkg/storage/utils/decomposedfs/node"
	"github.com/cs3org/reva/v2/pkg/storage/utils/decomposedfs/options"
	"github.com/cs3org/reva/v2/pkg/storage/utils/tus"
	"github.com/cs3org/reva/v2/pkg/utils"
	"github.com/golang-jwt/jwt"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	tusd "github.com/tus/tusd/pkg/handler"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer

func init() {
	tracer = otel.Tracer("github.com/cs3org/reva/pkg/storage/utils/decomposedfs/upload")
}

// Tree is used to manage a tree hierarchy
type Tree interface {
	Setup() error

	GetMD(ctx context.Context, node *node.Node) (os.FileInfo, error)
	ListFolder(ctx context.Context, node *node.Node) ([]*node.Node, error)
	// CreateHome(owner *userpb.UserId) (n *node.Node, err error)
	CreateDir(ctx context.Context, node *node.Node) (err error)
	// CreateReference(ctx context.Context, node *node.Node, targetURI *url.URL) error
	Move(ctx context.Context, oldNode *node.Node, newNode *node.Node) (err error)
	Delete(ctx context.Context, node *node.Node) (err error)
	RestoreRecycleItemFunc(ctx context.Context, spaceid, key, trashPath string, target *node.Node) (*node.Node, *node.Node, func() error, error)
	PurgeRecycleItemFunc(ctx context.Context, spaceid, key, purgePath string) (*node.Node, func() error, error)

	WriteBlob(node *node.Node, binPath string) error
	ReadBlob(node *node.Node) (io.ReadCloser, error)
	DeleteBlob(node *node.Node) error

	Propagate(ctx context.Context, node *node.Node, sizeDiff int64) (err error)
}

// Upload processes the upload
// it implements tus tusd.Upload interface https://tus.io/protocols/resumable-upload.html#core-protocol
// it also implements its termination extension as specified in https://tus.io/protocols/resumable-upload.html#termination
// it also implements its creation-defer-length extension as specified in https://tus.io/protocols/resumable-upload.html#creation
// it also implements its concatenation extension as specified in https://tus.io/protocols/resumable-upload.html#concatenation
type Upload struct {
	// we use a struct field on the upload as tus pkg will give us an empty context.Background
	Ctx context.Context
	// info stores the current information about the upload
	Session tus.Session
	// node for easy access
	Node *node.Node
	// lu and tp needed for file operations
	lu *lookup.Lookup
	tp Tree
	// and a logger as well
	log zerolog.Logger
	// publisher used to publish events
	pub events.Publisher
	// async determines if uploads shoud be done asynchronously
	async bool
	// tknopts hold token signing information
	tknopts options.TokenOptions
}

func buildUpload(ctx context.Context, session tus.Session, lu *lookup.Lookup, tp Tree, pub events.Publisher, async bool, tknopts options.TokenOptions) *Upload {
	return &Upload{
		Session: session,
		lu:      lu,
		tp:      tp,
		Ctx:     ctx,
		pub:     pub,
		async:   async,
		tknopts: tknopts,
		log: appctx.GetLogger(ctx).
			With().
			Interface("session", session).
			Logger(),
	}
}

// Cleanup cleans the upload
func Cleanup(upload *Upload, failure bool, keepUpload bool) {
	ctx, span := tracer.Start(upload.Ctx, "Cleanup")
	defer span.End()
	upload.cleanup(failure, !keepUpload, !keepUpload)

	// unset processing status
	if upload.Node != nil { // node can be nil when there was an error before it was created (eg. checksum-mismatch)
		if err := upload.Node.UnmarkProcessing(ctx, upload.Session.ID); err != nil {
			upload.log.Info().Str("path", upload.Node.InternalPath()).Err(err).Msg("unmarking processing failed")
		}
	}
}

// WriteChunk writes the stream from the reader to the given offset of the upload
func (upload *Upload) WriteChunk(_ context.Context, offset int64, src io.Reader) (int64, error) {
	ctx, span := tracer.Start(upload.Ctx, "WriteChunk")
	defer span.End()
	_, subspan := tracer.Start(ctx, "os.OpenFile")
	file, err := os.OpenFile(upload.Session.BinPath, os.O_WRONLY|os.O_APPEND, defaultFilePerm)
	subspan.End()
	if err != nil {
		return 0, err
	}
	defer file.Close()

	// calculate cheksum here? needed for the TUS checksum extension. https://tus.io/protocols/resumable-upload.html#checksum
	// TODO but how do we get the `Upload-Checksum`? WriteChunk() only has a context, offset and the reader ...
	// It is sent with the PATCH request, well or in the POST when the creation-with-upload extension is used
	// but the tus handler uses a context.Background() so we cannot really check the header and put it in the context ...
	_, subspan = tracer.Start(ctx, "io.Copy")
	n, err := io.Copy(file, src)
	subspan.End()

	// If the HTTP PATCH request gets interrupted in the middle (e.g. because
	// the user wants to pause the upload), Go's net/http returns an io.ErrUnexpectedEOF.
	// However, for the ocis driver it's not important whether the stream has ended
	// on purpose or accidentally.
	if err != nil && err != io.ErrUnexpectedEOF {
		return n, err
	}

	// update upload.Session.Offset so subsequent code flow can use it.
	// No need to persist the session as the offset is determined by stating the blob in the GetUpload codepath.
	// The session offset is written to disk in FinishUpload
	upload.Session.Offset += n
	return n, nil
}

// GetInfo returns the FileInfo
func (upload *Upload) GetInfo(_ context.Context) (tusd.FileInfo, error) {
	return upload.Session.ToFileInfo(), nil
}

// GetReader returns an io.Reader for the upload
func (upload *Upload) GetReader(_ context.Context) (io.Reader, error) {
	_, span := tracer.Start(upload.Ctx, "GetReader")
	defer span.End()
	return os.Open(upload.Session.BinPath)
}

// FinishUpload finishes an upload and moves the file to the internal destination
func (upload *Upload) FinishUpload(_ context.Context) error {
	ctx, span := tracer.Start(upload.Ctx, "FinishUpload")
	defer span.End()
	// set lockID to context
	if upload.Session.LockID != "" {
		upload.Ctx = ctxpkg.ContextSetLockID(upload.Ctx, upload.Session.LockID)
	}

	log := appctx.GetLogger(upload.Ctx)

	// calculate the checksum of the written bytes
	// they will all be written to the metadata later, so we cannot omit any of them
	// TODO only calculate the checksum in sync that was requested to match, the rest could be async ... but the tests currently expect all to be present
	// TODO the hashes all implement BinaryMarshaler so we could try to persist the state for resumable upload. we would neet do keep track of the copied bytes ...
	sha1h := sha1.New()
	md5h := md5.New()
	adler32h := adler32.New()
	{
		_, subspan := tracer.Start(ctx, "os.Open")
		f, err := os.Open(upload.Session.BinPath)
		subspan.End()
		if err != nil {
			// we can continue if no oc checksum header is set
			log.Info().Err(err).Str("binPath", upload.Session.BinPath).Msg("error opening binPath")
		}
		defer f.Close()

		r1 := io.TeeReader(f, sha1h)
		r2 := io.TeeReader(r1, md5h)

		_, subspan = tracer.Start(ctx, "io.Copy")
		_, err = io.Copy(adler32h, r2)
		subspan.End()
		if err != nil {
			log.Info().Err(err).Msg("error copying checksums")
		}
	}

	// compare if they match the sent checksum
	// TODO the tus checksum extension would do this on every chunk, but I currently don't see an easy way to pass in the requested checksum. for now we do it in FinishUpload which is also called for chunked uploads
	var err error
	switch {
	case upload.Session.ChecksumSHA1 != "":
		err = upload.checkHash(upload.Session.ChecksumSHA1, sha1h)
	case upload.Session.ChecksumMD5 != "":
		err = upload.checkHash(upload.Session.ChecksumMD5, md5h)
	case upload.Session.ChecksumADLER32 != "":
		err = upload.checkHash(upload.Session.ChecksumADLER32, adler32h)
	}
	if err != nil {
		Cleanup(upload, true, false)
		return err
	}

	// update checksums
	attrs := node.Attributes{
		prefixes.ChecksumPrefix + "sha1":    sha1h.Sum(nil),
		prefixes.ChecksumPrefix + "md5":     md5h.Sum(nil),
		prefixes.ChecksumPrefix + "adler32": adler32h.Sum(nil),
	}

	n, err := CreateNodeForUpload(upload, attrs)
	if err != nil {
		Cleanup(upload, true, false)
		return err
	}

	upload.Node = n

	if upload.pub != nil {
		u, _ := ctxpkg.ContextGetUser(upload.Ctx)
		s, err := upload.URL(upload.Ctx)
		if err != nil {
			return err
		}

		if err := events.Publish(ctx, upload.pub, events.BytesReceived{
			UploadID:      upload.Session.ID,
			URL:           s,
			SpaceOwner:    n.SpaceOwnerOrManager(upload.Ctx),
			ExecutingUser: u,
			ResourceID:    &provider.ResourceId{SpaceId: n.SpaceID, OpaqueId: n.ID},
			Filename:      upload.Session.Filename,
			Filesize:      uint64(upload.Session.Size),
		}); err != nil {
			return err
		}
	}

	if !upload.async {
		// handle postprocessing synchronously
		err = upload.Finalize()
		Cleanup(upload, err != nil, false)
		if err != nil {
			log.Error().Err(err).Msg("failed to upload")
			return err
		}
	}

	return upload.tp.Propagate(upload.Ctx, n, upload.Session.SizeDiff)
}

// Terminate terminates the upload
func (upload *Upload) Terminate(_ context.Context) error {
	upload.cleanup(true, true, true)
	return nil
}

// DeclareLength updates the upload length information
func (upload *Upload) DeclareLength(_ context.Context, length int64) error {
	upload.Session.Size = length
	upload.Session.SizeIsDeferred = false
	return upload.Session.Persist(upload.Ctx)
}

// ConcatUploads concatenates multiple uploads
func (upload *Upload) ConcatUploads(_ context.Context, uploads []tusd.Upload) (err error) {
	file, err := os.OpenFile(upload.Session.BinPath, os.O_WRONLY|os.O_APPEND, defaultFilePerm)
	if err != nil {
		return err
	}
	defer file.Close()

	for _, partialUpload := range uploads {
		fileUpload := partialUpload.(*Upload)

		src, err := os.Open(fileUpload.Session.BinPath)
		if err != nil {
			return err
		}
		defer src.Close()

		if _, err := io.Copy(file, src); err != nil {
			return err
		}
	}

	return
}

// Finalize finalizes the upload (eg moves the file to the internal destination)
func (upload *Upload) Finalize() (err error) {
	ctx, span := tracer.Start(upload.Ctx, "Finalize")
	defer span.End()
	n := upload.Node
	if n == nil {
		var err error
		n, err = node.ReadNode(ctx, upload.lu, upload.Session.SpaceRoot, upload.Session.NodeID, false, nil, false)
		if err != nil {
			return err
		}
		upload.Node = n
	}

	// upload the data to the blobstore
	_, subspan := tracer.Start(ctx, "WriteBlob")
	err = upload.tp.WriteBlob(n, upload.Session.BinPath)
	subspan.End()
	if err != nil {
		return errors.Wrap(err, "failed to upload file to blobstore")
	}

	return nil
}

func (upload *Upload) checkHash(expected string, h hash.Hash) error {
	hash := hex.EncodeToString(h.Sum(nil))
	if expected != hash {
		return errtypes.ChecksumMismatch(fmt.Sprintf("invalid checksum: expected %s got %x", expected, hash))
	}
	return nil
}

// cleanup cleans up after the upload is finished
func (upload *Upload) cleanup(cleanNode, cleanBin, cleanInfo bool) {
	if cleanNode && upload.Node != nil {
		switch p := upload.Session.VersionsPath; p {
		case "":
			// remove node
			if err := utils.RemoveItem(upload.Node.InternalPath()); err != nil {
				upload.log.Info().Str("path", upload.Node.InternalPath()).Err(err).Msg("removing node failed")
			}

			// no old version was present - remove child entry
			src := filepath.Join(upload.Node.ParentPath(), upload.Node.Name)
			if err := os.Remove(src); err != nil {
				upload.log.Info().Str("path", upload.Node.ParentPath()).Err(err).Msg("removing node from parent failed")
			}

			// remove node from upload as it no longer exists
			upload.Node = nil
		default:

			if err := upload.lu.CopyMetadata(upload.Ctx, p, upload.Node.InternalPath(), func(attributeName string, value []byte) (newValue []byte, copy bool) {
				return value, strings.HasPrefix(attributeName, prefixes.ChecksumPrefix) ||
					attributeName == prefixes.TypeAttr ||
					attributeName == prefixes.BlobIDAttr ||
					attributeName == prefixes.BlobsizeAttr ||
					attributeName == prefixes.MTimeAttr
			}, true); err != nil {
				upload.log.Info().Str("versionpath", p).Str("nodepath", upload.Node.InternalPath()).Err(err).Msg("renaming version node failed")
			}

			if err := os.RemoveAll(p); err != nil {
				upload.log.Info().Str("versionpath", p).Str("nodepath", upload.Node.InternalPath()).Err(err).Msg("error removing version")
			}

		}
	}

	if cleanBin {
		if err := os.Remove(upload.Session.BinPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			upload.log.Error().Str("path", upload.Session.BinPath).Err(err).Msg("removing upload failed")
		}
	}

	if cleanInfo {
		if err := upload.Session.Purge(upload.Ctx); err != nil && !errors.Is(err, fs.ErrNotExist) {
			upload.log.Error().Err(err).Str("session", upload.Session.ID).Msg("removing upload info failed")
		}
	}
}

// URL returns a url to download an upload
func (upload *Upload) URL(_ context.Context) (string, error) {
	type transferClaims struct {
		jwt.StandardClaims
		Target string `json:"target"`
	}

	u := joinurl(upload.tknopts.DownloadEndpoint, "tus/", upload.Session.ID)
	ttl := time.Duration(upload.tknopts.TransferExpires) * time.Second
	claims := transferClaims{
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: time.Now().Add(ttl).Unix(),
			Audience:  "reva",
			IssuedAt:  time.Now().Unix(),
		},
		Target: u,
	}

	t := jwt.NewWithClaims(jwt.GetSigningMethod("HS256"), claims)

	tkn, err := t.SignedString([]byte(upload.tknopts.TransferSharedSecret))
	if err != nil {
		return "", errors.Wrapf(err, "error signing token with claims %+v", claims)
	}

	return joinurl(upload.tknopts.DataGatewayEndpoint, tkn), nil
}

// replace with url.JoinPath after switching to go1.19
func joinurl(paths ...string) string {
	var s strings.Builder
	l := len(paths)
	for i, p := range paths {
		s.WriteString(p)
		if !strings.HasSuffix(p, "/") && i != l-1 {
			s.WriteString("/")
		}
	}

	return s.String()
}
