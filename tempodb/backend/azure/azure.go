package azure

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/go-kit/log/level"
	"github.com/google/uuid"
	"github.com/grafana/tempo/pkg/util/log"
	"github.com/grafana/tempo/tempodb/backend"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// dir represents the char separator used by the blob virtual directory structure
	dir = "/"
	// max parallelism on uploads
	maxParallelism = 3
)

type Azure struct {
	cfg                   *Config
	containerClient       *container.Client
	hedgedContainerClient *container.Client
}

var (
	_ backend.RawReader             = (*Azure)(nil)
	_ backend.RawWriter             = (*Azure)(nil)
	_ backend.Compactor             = (*Azure)(nil)
	_ backend.VersionedReaderWriter = (*Azure)(nil)
)

type appendTracker struct {
	Name string
}

var tracer = otel.Tracer("tempodb/backend/azure")

// NewNoConfirm gets the Azure blob container without testing it
func NewNoConfirm(cfg *Config) (backend.RawReader, backend.RawWriter, backend.Compactor, error) {
	rw, err := internalNew(cfg, false)
	return rw, rw, rw, err
}

// New gets the Azure blob container
func New(cfg *Config) (backend.RawReader, backend.RawWriter, backend.Compactor, error) {
	rw, err := internalNew(cfg, true)
	return rw, rw, rw, err
}

// NewVersionedReaderWriter creates a client to perform versioned requests. Note that write requests are
// best-effort for now. We need to update the SDK to make use of the precondition headers.
// https://github.com/grafana/tempo/issues/2705
func NewVersionedReaderWriter(cfg *Config) (backend.VersionedReaderWriter, error) {
	return internalNew(cfg, true)
}

func internalNew(cfg *Config, confirm bool) (*Azure, error) {
	ctx := context.Background()

	c, err := getContainerClient(ctx, cfg, false)
	if err != nil {
		return nil, fmt.Errorf("getting storage container: %w", err)
	}

	hedgedContainer, err := getContainerClient(ctx, cfg, true)
	if err != nil {
		return nil, fmt.Errorf("getting hedged storage container: %w", err)
	}

	if confirm {
		// Getting container properties to check if container exists
		_, err = c.GetProperties(ctx, &container.GetPropertiesOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to GetProperties: %w", err)
		}
	}

	rw := &Azure{
		cfg:                   cfg,
		containerClient:       c,
		hedgedContainerClient: hedgedContainer,
	}

	return rw, nil
}

func readError(err error) error {
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return backend.ErrDoesNotExist
	}

	if err != nil {
		return fmt.Errorf("reading Azure blob container: %w", err)
	}
	return nil
}

// Write implements backend.Writer
func (rw *Azure) Write(ctx context.Context, name string, keypath backend.KeyPath, data io.Reader, _ int64, _ *backend.CacheInfo) error {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)

	derivedCtx, span := tracer.Start(ctx, "azure.Write")
	defer span.End()

	return rw.writer(derivedCtx, bufio.NewReader(data), backend.ObjectFileName(keypath, name))
}

// Append implements backend.Writer
func (rw *Azure) Append(ctx context.Context, name string, keypath backend.KeyPath, tracker backend.AppendTracker, buffer []byte) (backend.AppendTracker, error) {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)
	var a appendTracker
	if tracker == nil {
		a.Name = backend.ObjectFileName(keypath, name)

		err := rw.writeAll(ctx, a.Name, buffer)
		if err != nil {
			return nil, err
		}
	} else {
		a = tracker.(appendTracker)

		err := rw.append(ctx, buffer, a.Name)
		if err != nil {
			return nil, err
		}
	}

	return a, nil
}

// CloseAppend implements backend.Writer
func (rw *Azure) CloseAppend(context.Context, backend.AppendTracker) error {
	return nil
}

func (rw *Azure) Delete(ctx context.Context, name string, keypath backend.KeyPath, _ *backend.CacheInfo) error {
	blobClient, err := getBlobClient(ctx, rw.cfg, backend.ObjectFileName(keypath, name))
	if err != nil {
		return fmt.Errorf("cannot get Azure blob client, name: %s: %w", backend.ObjectFileName(keypath, name), err)
	}

	snapshotType := blob.DeleteSnapshotsOptionTypeInclude
	if _, err = blobClient.Delete(ctx, &blob.DeleteOptions{DeleteSnapshots: &snapshotType}); err != nil {
		return readError(err)
	}
	return nil
}

// List implements backend.Reader
func (rw *Azure) List(ctx context.Context, keypath backend.KeyPath) ([]string, error) {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)

	prefix := path.Join(keypath...)

	if len(prefix) > 0 {
		prefix = prefix + dir
	}

	pager := rw.containerClient.NewListBlobsHierarchyPager(dir, &container.ListBlobsHierarchyOptions{
		Include: container.ListBlobsInclude{},
		Prefix:  &prefix,
	})

	objects := make([]string, 0)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return objects, fmt.Errorf("iterating tenants: %w", err)
		}

		for _, b := range page.Segment.BlobPrefixes {
			if b.Name == nil {
				return objects, fmt.Errorf("unexpected empty blob name when listing %s: %w", prefix, err)
			}
			objects = append(objects, strings.TrimPrefix(strings.TrimSuffix(*b.Name, dir), prefix))
		}
	}
	return objects, nil
}

// ListBlocks implements backend.Reader
func (rw *Azure) ListBlocks(ctx context.Context, tenant string) ([]uuid.UUID, []uuid.UUID, error) {
	ctx, span := tracer.Start(ctx, "V2.ListBlocks")
	defer span.End()

	var (
		blockIDs          = make([]uuid.UUID, 0, 1000)
		compactedBlockIDs = make([]uuid.UUID, 0, 1000)
		keypath           = backend.KeyPathWithPrefix(backend.KeyPath{tenant}, rw.cfg.Prefix)
		parts             []string
		id                uuid.UUID
	)

	prefix := path.Join(keypath...)
	if len(prefix) > 0 {
		prefix += dir
	}

	pager := rw.containerClient.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Include: container.ListBlobsInclude{},
		Prefix:  &prefix,
	})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("iterating objects: %w", err)
		}

		for _, b := range page.Segment.BlobItems {
			if b.Name == nil {
				continue
			}

			obj := strings.TrimPrefix(strings.TrimSuffix(*b.Name, dir), prefix)
			parts = strings.Split(obj, "/")

			// ie: <blockID>/meta.json
			if len(parts) != 2 {
				continue
			}

			if parts[1] != backend.MetaName && parts[1] != backend.CompactedMetaName {
				continue
			}

			id, err = uuid.Parse(parts[0])
			if err != nil {
				return nil, nil, err
			}

			switch parts[1] {
			case backend.MetaName:
				blockIDs = append(blockIDs, id)
			case backend.CompactedMetaName:
				compactedBlockIDs = append(compactedBlockIDs, id)
			}
		}
	}
	return blockIDs, compactedBlockIDs, nil
}

// Find implements backend.Reader
func (rw *Azure) Find(ctx context.Context, keypath backend.KeyPath, f backend.FindFunc) (err error) {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)

	prefix := path.Join(keypath...)
	if len(prefix) > 0 {
		prefix = prefix + dir
	}

	pager := rw.containerClient.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix: &prefix,
	})

	var o string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("iterating objects: %w", err)
		}

		for _, b := range page.Segment.BlobItems {
			if b == nil || b.Name == nil {
				continue
			}

			o = strings.TrimSuffix(*b.Name, dir)
			opts := backend.FindMatch{
				Key:      o,
				Modified: *b.Properties.LastModified,
			}
			f(opts)
		}

	}

	return
}

// Read implements backend.Reader
func (rw *Azure) Read(ctx context.Context, name string, keypath backend.KeyPath, _ *backend.CacheInfo) (io.ReadCloser, int64, error) {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)

	derivedCtx, span := tracer.Start(ctx, "azure.Read")
	defer span.End()

	object := backend.ObjectFileName(keypath, name)
	b, _, err := rw.readAll(derivedCtx, object)
	if err != nil {
		return nil, 0, readError(err)
	}

	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// ReadRange implements backend.Reader
func (rw *Azure) ReadRange(ctx context.Context, name string, keypath backend.KeyPath, offset uint64, buffer []byte, _ *backend.CacheInfo) error {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)

	derivedCtx, span := tracer.Start(ctx, "azure.ReadRange", trace.WithAttributes(
		attribute.Int("len", len(buffer)),
		attribute.Int64("offset", int64(offset)),
	))
	defer span.End()

	object := backend.ObjectFileName(keypath, name)
	err := rw.readRange(derivedCtx, object, int64(offset), buffer)
	if err != nil {
		return readError(err)
	}

	return nil
}

// Shutdown implements backend.Reader
func (rw *Azure) Shutdown() {
}

func (rw *Azure) WriteVersioned(ctx context.Context, name string, keypath backend.KeyPath, data io.Reader, size int64, version backend.Version) (backend.Version, error) {
	// TODO use conditional if-match API
	_, currentVersion, err := rw.ReadVersioned(ctx, name, keypath)
	if err != nil && !errors.Is(err, backend.ErrDoesNotExist) {
		return "", err
	}

	level.Info(log.Logger).Log("msg", "WriteVersioned - fetching data", "currentVersion", currentVersion, "err", err, "version", version)

	// object does not exist - supplied version must be "0"
	if errors.Is(err, backend.ErrDoesNotExist) && version != backend.VersionNew {
		return "", backend.ErrVersionDoesNotMatch
	}
	if !errors.Is(err, backend.ErrDoesNotExist) && version != currentVersion {
		return "", backend.ErrVersionDoesNotMatch
	}

	err = rw.Write(ctx, name, keypath, data, size, nil)
	if err != nil {
		return "", err
	}

	_, currentVersion, err = rw.ReadVersioned(ctx, name, keypath)
	return currentVersion, err
}

func (rw *Azure) DeleteVersioned(ctx context.Context, name string, keypath backend.KeyPath, version backend.Version) error {
	// TODO use conditional if-match API
	_, currentVersion, err := rw.ReadVersioned(ctx, name, keypath)
	if err != nil && !errors.Is(err, backend.ErrDoesNotExist) {
		return err
	}
	if !errors.Is(err, backend.ErrDoesNotExist) && currentVersion != version {
		return backend.ErrVersionDoesNotMatch
	}

	return rw.Delete(ctx, name, keypath, nil)
}

func (rw *Azure) ReadVersioned(ctx context.Context, name string, keypath backend.KeyPath) (io.ReadCloser, backend.Version, error) {
	keypath = backend.KeyPathWithPrefix(keypath, rw.cfg.Prefix)

	derivedCtx, span := tracer.Start(ctx, "azure.ReadVersioned")
	defer span.End()

	object := backend.ObjectFileName(keypath, name)
	b, etag, err := rw.readAll(derivedCtx, object)
	if err != nil {
		return nil, "", readError(err)
	}

	return io.NopCloser(bytes.NewReader(b)), backend.Version(etag), nil
}

func (rw *Azure) writeAll(ctx context.Context, name string, b []byte) error {
	err := rw.writer(ctx, bytes.NewReader(b), name)
	if err != nil {
		return err
	}

	return nil
}

func (rw *Azure) append(ctx context.Context, src []byte, name string) error {
	appendBlobClient := rw.containerClient.NewBlockBlobClient(name)

	// These helper functions convert a binary block ID to a base-64 string and vice versa
	// NOTE: The blockID must be <= 64 bytes and ALL blockIDs for the block must be the same length
	blockIDBinaryToBase64 := func(blockID []byte) string { return base64.StdEncoding.EncodeToString(blockID) }

	blockIDIntToBase64 := func(blockID int) string {
		binaryBlockID := (&[64]byte{})[:]
		binary.LittleEndian.PutUint32(binaryBlockID, uint32(blockID))
		return blockIDBinaryToBase64(binaryBlockID)
	}

	l, err := appendBlobClient.GetBlockList(ctx, blockblob.BlockListTypeAll, &blockblob.GetBlockListOptions{})
	if err != nil {
		return err
	}

	// generate the next block id
	id := blockIDIntToBase64(len(l.CommittedBlocks) + 1)

	_, err = appendBlobClient.StageBlock(ctx, id, streaming.NopCloser(bytes.NewReader(src)), &blockblob.StageBlockOptions{})
	if err != nil {
		return err
	}

	base64BlockIDs := make([]string, len(l.CommittedBlocks)+1)
	for i := 0; i < len(l.CommittedBlocks); i++ {
		base64BlockIDs[i] = *l.CommittedBlocks[i].Name
	}

	base64BlockIDs[len(l.CommittedBlocks)] = id

	// After all the blocks are uploaded, atomically commit them to the blob.
	_, err = appendBlobClient.CommitBlockList(ctx, base64BlockIDs, &blockblob.CommitBlockListOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (rw *Azure) writer(ctx context.Context, src io.Reader, name string) error {
	blobClient := rw.containerClient.NewBlockBlobClient(name)

	_, err := blobClient.UploadStream(ctx, src, &azblob.UploadStreamOptions{
		BlockSize:   int64(rw.cfg.BufferSize),
		Concurrency: rw.cfg.MaxBuffers,
	})
	if err != nil {
		return fmt.Errorf("cannot upload blob, name: %s: %w", name, err)
	}

	return nil
}

func (rw *Azure) readRange(ctx context.Context, name string, offset int64, destBuffer []byte) error {
	blobClient := rw.hedgedContainerClient.NewBlockBlobClient(name)

	props, err := blobClient.GetProperties(ctx, &blob.GetPropertiesOptions{})
	if err != nil {
		return err
	}

	length := int64(len(destBuffer))
	var size int64

	if props.ContentLength == nil {
		return fmt.Errorf("expected content length but got none for blob %s: %w", name, err)
	}

	if length > 0 && length <= *props.ContentLength-offset {
		size = length
	} else {
		size = *props.ContentLength - offset
	}

	if _, err := blobClient.DownloadBuffer(ctx, destBuffer, &blob.DownloadBufferOptions{
		Range: blob.HTTPRange{
			Offset: offset,
			Count:  size,
		},
		BlockSize:   blob.DefaultDownloadBlockSize,
		Concurrency: maxParallelism,
		RetryReaderOptionsPerBlock: blob.RetryReaderOptions{
			MaxRetries: maxRetries,
		},
	}); err != nil {
		return err
	}

	_, err = bytes.NewReader(destBuffer).Read(destBuffer)
	if err != nil {
		return err
	}

	return nil
}

func (rw *Azure) readAll(ctx context.Context, name string) ([]byte, azcore.ETag, error) {
	blobClient := rw.hedgedContainerClient.NewBlockBlobClient(name)

	props, err := blobClient.GetProperties(ctx, &blob.GetPropertiesOptions{})
	if err != nil {
		return nil, "", err
	}

	if props.ContentLength == nil {
		return nil, "", fmt.Errorf("expected content length but got none for blob %s: %w", name, err)
	}

	destBuffer := make([]byte, *props.ContentLength)

	if _, err := blobClient.DownloadBuffer(context.Background(), destBuffer, &blob.DownloadBufferOptions{
		Range: blob.HTTPRange{
			Offset: 0,
			Count:  *props.ContentLength,
		},
		BlockSize:   blob.DefaultDownloadBlockSize,
		Concurrency: maxParallelism,
		RetryReaderOptionsPerBlock: blob.RetryReaderOptions{
			MaxRetries: maxRetries,
		},
	}); err != nil {
		return nil, "", err
	}

	var etag azcore.ETag
	if props.ETag != nil {
		etag = *props.ETag
	}

	return destBuffer, etag, nil
}
