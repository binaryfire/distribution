package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/distribution/v3/internal/uuid"
	"github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// linkPathFunc describes a function that can resolve a link based on the
// repository name and digest.
type linkPathFunc func(name string, dgst digest.Digest) (string, error)

// linkedBlobStore provides a full BlobService that namespaces the blobs to a
// given repository. Effectively, it manages the links in a given repository
// that grant access to the global blob store.
type linkedBlobStore struct {
	*blobStore
	registry               *registry
	blobServer             distribution.BlobServer
	blobAccessController   distribution.BlobDescriptorService
	repository             distribution.Repository
	ctx                    context.Context // only to be used where context can't come through method args
	deleteEnabled          bool
	resumableDigestEnabled bool

	// linkPath allows one to control the repository blob link set to which
	// the blob store dispatches. This is required because manifest and layer
	// blobs have not yet been fully merged. At some point, this functionality
	// should be removed and the blob links folder should be merged.
	linkPath linkPathFunc

	// linkDirectoryPathSpec locates the root directories in which one might find links
	linkDirectoryPathSpec pathSpec
}

var _ distribution.BlobStore = &linkedBlobStore{}

func (lbs *linkedBlobStore) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	return lbs.blobAccessController.Stat(ctx, dgst)
}

func (lbs *linkedBlobStore) Get(ctx context.Context, dgst digest.Digest) ([]byte, error) {
	canonical, err := lbs.Stat(ctx, dgst) // access check
	if err != nil {
		return nil, err
	}

	return lbs.blobStore.Get(ctx, canonical.Digest)
}

func (lbs *linkedBlobStore) Open(ctx context.Context, dgst digest.Digest) (io.ReadSeekCloser, error) {
	canonical, err := lbs.Stat(ctx, dgst) // access check
	if err != nil {
		return nil, err
	}

	return lbs.blobStore.Open(ctx, canonical.Digest)
}

func (lbs *linkedBlobStore) ServeBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest) error {
	canonical, err := lbs.Stat(ctx, dgst) // access check
	if err != nil {
		return err
	}

	if canonical.MediaType != "" {
		// Set the repository local content type.
		w.Header().Set("Content-Type", canonical.MediaType)
	}

	return lbs.blobServer.ServeBlob(ctx, w, r, canonical.Digest)
}

func (lbs *linkedBlobStore) Put(ctx context.Context, mediaType string, p []byte) (v1.Descriptor, error) {
	dgst := digest.FromBytes(p)
	// Place the data in the blob store first.
	desc, err := lbs.blobStore.Put(ctx, mediaType, p)
	if err != nil {
		dcontext.GetLogger(ctx).Errorf("error putting into main store: %v", err)
		return v1.Descriptor{}, err
	}

	if err := lbs.blobAccessController.SetDescriptor(ctx, dgst, desc); err != nil {
		return v1.Descriptor{}, err
	}

	// TODO(stevvooe): Write out mediatype if incoming differs from what is
	// returned by Put above. Note that we should allow updates for a given
	// repository.

	return desc, lbs.linkBlob(ctx, desc)
}

type optionFunc func(interface{}) error

func (f optionFunc) Apply(v interface{}) error {
	return f(v)
}

// WithMountFrom returns a BlobCreateOption which designates that the blob should be
// mounted from the given canonical reference.
func WithMountFrom(ref reference.Canonical) distribution.BlobCreateOption {
	return optionFunc(func(v interface{}) error {
		opts, ok := v.(*distribution.CreateOptions)
		if !ok {
			return fmt.Errorf("unexpected options type: %T", v)
		}

		opts.Mount.ShouldMount = true
		opts.Mount.From = ref

		return nil
	})
}

// Create begins a blob write session, returning a handle.
func (lbs *linkedBlobStore) Create(ctx context.Context, options ...distribution.BlobCreateOption) (distribution.BlobWriter, error) {
	dcontext.GetLogger(ctx).Debug("(*linkedBlobStore).Create")

	var opts distribution.CreateOptions

	for _, option := range options {
		err := option.Apply(&opts)
		if err != nil {
			return nil, err
		}
	}

	if opts.Mount.ShouldMount {
		desc, err := lbs.mount(ctx, opts.Mount.From, opts.Mount.From.Digest(), opts.Mount.Stat)
		if err == nil {
			// Mount successful, no need to initiate an upload session
			return nil, distribution.ErrBlobMounted{From: opts.Mount.From, Descriptor: desc}
		}
	}

	uuid := uuid.NewString()
	startedAt := time.Now().UTC()

	path, err := pathFor(uploadDataPathSpec{
		name: lbs.repository.Named().Name(),
		id:   uuid,
	})
	if err != nil {
		return nil, err
	}

	startedAtPath, err := pathFor(uploadStartedAtPathSpec{
		name: lbs.repository.Named().Name(),
		id:   uuid,
	})
	if err != nil {
		return nil, err
	}

	// Write a startedat file for this upload
	if err := lbs.blobStore.driver.PutContent(ctx, startedAtPath, []byte(startedAt.Format(time.RFC3339))); err != nil {
		return nil, err
	}

	return lbs.newBlobUpload(ctx, uuid, path, startedAt, false)
}

func (lbs *linkedBlobStore) Resume(ctx context.Context, id string) (distribution.BlobWriter, error) {
	dcontext.GetLogger(ctx).Debug("(*linkedBlobStore).Resume")

	startedAtPath, err := pathFor(uploadStartedAtPathSpec{
		name: lbs.repository.Named().Name(),
		id:   id,
	})
	if err != nil {
		return nil, err
	}

	startedAtBytes, err := lbs.blobStore.driver.GetContent(ctx, startedAtPath)
	if err != nil {
		switch err := err.(type) {
		case driver.PathNotFoundError:
			return nil, distribution.ErrBlobUploadUnknown
		default:
			return nil, err
		}
	}

	startedAt, err := time.Parse(time.RFC3339, string(startedAtBytes))
	if err != nil {
		return nil, err
	}

	path, err := pathFor(uploadDataPathSpec{
		name: lbs.repository.Named().Name(),
		id:   id,
	})
	if err != nil {
		return nil, err
	}

	return lbs.newBlobUpload(ctx, id, path, startedAt, true)
}

func (lbs *linkedBlobStore) Delete(ctx context.Context, dgst digest.Digest) error {
	if !lbs.deleteEnabled {
		return distribution.ErrUnsupported
	}

	// Ensure the blob is available for deletion
	_, err := lbs.blobAccessController.Stat(ctx, dgst)
	if err != nil {
		return err
	}

	err = lbs.blobAccessController.Clear(ctx, dgst)
	if err != nil {
		return err
	}

	return nil
}

func (lbs *linkedBlobStore) Enumerate(ctx context.Context, ingestor func(digest.Digest) error) error {
	rootPath, err := pathFor(lbs.linkDirectoryPathSpec)
	if err != nil {
		return err
	}
	return lbs.driver.Walk(ctx, rootPath, func(fileInfo driver.FileInfo) error {
		// exit early if directory...
		if fileInfo.IsDir() {
			return nil
		}
		filePath := fileInfo.Path()

		// check if it's a link
		_, fileName := path.Split(filePath)
		if fileName != "link" {
			return nil
		}

		// read the digest found in link
		digest, err := lbs.blobStore.readlink(ctx, filePath)
		if err != nil {
			return err
		}

		// ensure this conforms to the linkPathFns
		_, err = lbs.Stat(ctx, digest)
		if err != nil {
			// we expect this error to occur so we move on
			if err == distribution.ErrBlobUnknown {
				return nil
			}
			return err
		}

		err = ingestor(digest)
		if err != nil {
			return err
		}

		return nil
	})
}

func (lbs *linkedBlobStore) mount(ctx context.Context, sourceRepo reference.Named, dgst digest.Digest, sourceStat *v1.Descriptor) (v1.Descriptor, error) {
	var stat v1.Descriptor
	if sourceStat == nil {
		// look up the blob info from the sourceRepo if not already provided
		repo, err := lbs.registry.Repository(ctx, sourceRepo)
		if err != nil {
			return v1.Descriptor{}, err
		}
		stat, err = repo.Blobs(ctx).Stat(ctx, dgst)
		if err != nil {
			return v1.Descriptor{}, err
		}
	} else {
		// use the provided blob info
		stat = *sourceStat
	}

	desc := v1.Descriptor{
		Size: stat.Size,

		// NOTE(stevvooe): The central blob store firewalls media types from
		// other users. The caller should look this up and override the value
		// for the specific repository.
		MediaType: "application/octet-stream",
		Digest:    dgst,
	}
	return desc, lbs.linkBlob(ctx, desc)
}

// newBlobUpload allocates a new upload controller with the given state.
func (lbs *linkedBlobStore) newBlobUpload(ctx context.Context, uuid, path string, startedAt time.Time, append bool) (distribution.BlobWriter, error) {
	fw, err := lbs.driver.Writer(ctx, path, append)
	if err != nil {
		return nil, err
	}

	bw := &blobWriter{
		ctx:                    ctx,
		blobStore:              lbs,
		id:                     uuid,
		startedAt:              startedAt,
		digester:               digest.Canonical.Digester(),
		fileWriter:             fw,
		driver:                 lbs.driver,
		path:                   path,
		resumableDigestEnabled: lbs.resumableDigestEnabled,
	}

	return bw, nil
}

// linkBlob links a valid, written blob into the registry under the named
// repository for the upload controller.
func (lbs *linkedBlobStore) linkBlob(ctx context.Context, canonical v1.Descriptor, aliases ...digest.Digest) error {
	dgsts := append([]digest.Digest{canonical.Digest}, aliases...)

	// TODO(stevvooe): Need to write out mediatype for only canonical hash
	// since we don't care about the aliases. They are generally unused except
	// for tarsum but those versions don't care about mediatype.

	// Don't make duplicate links.
	seenDigests := make(map[digest.Digest]struct{}, len(dgsts))

	for _, dgst := range dgsts {
		if _, seen := seenDigests[dgst]; seen {
			continue
		}
		seenDigests[dgst] = struct{}{}

		blobLinkPath, err := lbs.linkPath(lbs.repository.Named().Name(), dgst)
		if err != nil {
			return err
		}

		if err := lbs.blobStore.link(ctx, blobLinkPath, canonical.Digest); err != nil {
			return err
		}
	}

	return nil
}

type linkedBlobStatter struct {
	*blobStore
	repository distribution.Repository

	// linkPath allows one to control the repository blob link set to which
	// the blob store dispatches. This is required because manifest and layer
	// blobs have not yet been fully merged. At some point, this functionality
	// should be removed an the blob links folder should be merged.
	linkPath linkPathFunc
}

var _ distribution.BlobDescriptorService = &linkedBlobStatter{}

func (lbs *linkedBlobStatter) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	blobLinkPath, err := lbs.linkPath(lbs.repository.Named().Name(), dgst)
	if err != nil {
		return v1.Descriptor{}, err
	}

	target, err := lbs.blobStore.readlink(ctx, blobLinkPath)
	if err != nil {
		switch err := err.(type) {
		case driver.PathNotFoundError:
			return v1.Descriptor{}, distribution.ErrBlobUnknown
		default:
			return v1.Descriptor{}, err
		}
	}

	if target != dgst {
		// Track when we are doing cross-digest domain lookups. ie, sha512 to sha256.
		dcontext.GetLogger(ctx).Warnf("looking up blob with canonical target: %v -> %v", dgst, target)
	}

	// TODO(stevvooe): Look up repository local mediatype and replace that on
	// the returned descriptor.

	return lbs.blobStore.statter.Stat(ctx, target)
}

func (lbs *linkedBlobStatter) Clear(ctx context.Context, dgst digest.Digest) (err error) {
	blobLinkPath, err := lbs.linkPath(lbs.repository.Named().Name(), dgst)
	if err != nil {
		return err
	}

	return lbs.blobStore.driver.Delete(ctx, blobLinkPath)
}

func (lbs *linkedBlobStatter) SetDescriptor(ctx context.Context, dgst digest.Digest, desc v1.Descriptor) error {
	// The canonical descriptor for a blob is set at the commit phase of upload
	return nil
}

// blobLinkPath provides the path to the blob link, also known as layers.
func blobLinkPath(name string, dgst digest.Digest) (string, error) {
	return pathFor(layerLinkPathSpec{name: name, digest: dgst})
}

// manifestRevisionLinkPath provides the path to the manifest revision link.
func manifestRevisionLinkPath(name string, dgst digest.Digest) (string, error) {
	return pathFor(manifestRevisionLinkPathSpec{name: name, revision: dgst})
}
