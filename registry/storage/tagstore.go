package storage

import (
	"context"
	"os"
	"path"
	"sort"
	"strconv"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/internal/dcontext"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"
)

var _ distribution.TagService = &TagStore{}

// TagStore provides methods to manage manifest tags in a backend storage driver.
// This implementation uses the same on-disk layout as the (now deleted) tag
// store.  This provides backward compatibility with current registry deployments
// which only makes use of the Digest field of the returned distribution.Descriptor
// but does not enable full roundtripping of Descriptor objects
type TagStore struct {
	repository              *repository
	blobStore               *blobStore
	lookupConcurrencyFactor int
}

func NewStore(ctx context.Context, repository *repository, blobStore *blobStore) *TagStore {
	logger := dcontext.GetLogger(ctx)
	lookupConcurrencyFactor, err := strconv.Atoi(os.Getenv("STORAGE_TAGSTORE_LOOKUP_CONCURRENCY"))
	if err != nil {
		lookupConcurrencyFactor = 64
		logger.Infof("TagStore: STORAGE_TAGSTORE_LOOKUP_CONCURRENCY is not set. Using default %d as lookup concurrency factor", lookupConcurrencyFactor)
	}
	return &TagStore{
		repository:              repository,
		blobStore:               blobStore,
		lookupConcurrencyFactor: lookupConcurrencyFactor,
	}
}

// All returns all tags
func (ts *TagStore) All(ctx context.Context) ([]string, error) {
	pathSpec, err := pathFor(manifestTagsPathSpec{
		name: ts.repository.Named().Name(),
	})
	if err != nil {
		return nil, err
	}

	entries, err := ts.blobStore.driver.List(ctx, pathSpec)
	if err != nil {
		switch err := err.(type) {
		case storagedriver.PathNotFoundError:
			return nil, distribution.ErrRepositoryUnknown{Name: ts.repository.Named().Name()}
		default:
			return nil, err
		}
	}

	tags := make([]string, 0, len(entries))
	for _, entry := range entries {
		_, filename := path.Split(entry)
		tags = append(tags, filename)
	}

	// there is no guarantee for the order,
	// therefore sort before return.
	sort.Strings(tags)

	return tags, nil
}

// Tag tags the digest with the given tag, updating the store to point at
// the current tag. The digest must point to a manifest.
func (ts *TagStore) Tag(ctx context.Context, tag string, desc distribution.Descriptor) error {
	currentPath, err := pathFor(manifestTagCurrentPathSpec{
		name: ts.repository.Named().Name(),
		tag:  tag,
	})
	if err != nil {
		return err
	}

	lbs := ts.linkedBlobStore(ctx, tag)

	// Link into the index
	if err := lbs.linkBlob(ctx, desc); err != nil {
		return err
	}

	// Overwrite the current link
	return ts.blobStore.link(ctx, currentPath, desc.Digest)
}

// resolve the current revision for name and tag.
func (ts *TagStore) Get(ctx context.Context, tag string) (distribution.Descriptor, error) {
	currentPath, err := pathFor(manifestTagCurrentPathSpec{
		name: ts.repository.Named().Name(),
		tag:  tag,
	})
	if err != nil {
		return distribution.Descriptor{}, err
	}

	revision, err := ts.blobStore.readlink(ctx, currentPath)
	if err != nil {
		switch err.(type) {
		case storagedriver.PathNotFoundError:
			return distribution.Descriptor{}, distribution.ErrTagUnknown{Tag: tag}
		}

		return distribution.Descriptor{}, err
	}

	return distribution.Descriptor{Digest: revision}, nil
}

// Untag removes the tag association
func (ts *TagStore) Untag(ctx context.Context, tag string) error {
	tagPath, err := pathFor(manifestTagPathSpec{
		name: ts.repository.Named().Name(),
		tag:  tag,
	})
	if err != nil {
		return err
	}

	return ts.blobStore.driver.Delete(ctx, tagPath)
}

// linkedBlobStore returns the linkedBlobStore for the named tag, allowing one
// to index manifest blobs by tag name. While the tag store doesn't map
// precisely to the linked blob store, using this ensures the links are
// managed via the same code path.
func (ts *TagStore) linkedBlobStore(ctx context.Context, tag string) *linkedBlobStore {
	return &linkedBlobStore{
		blobStore:  ts.blobStore,
		repository: ts.repository,
		ctx:        ctx,
		linkPath: func(name string, dgst digest.Digest) (string, error) {
			return pathFor(manifestTagIndexEntryLinkPathSpec{
				name:     name,
				tag:      tag,
				revision: dgst,
			})
		},
	}
}

// Lookup recovers a list of tags which refer to this digest.  When a manifest is deleted by
// digest, tag entries which point to it need to be recovered to avoid dangling tags.
func (ts *TagStore) Lookup(ctx context.Context, desc distribution.Descriptor) ([]string, error) {
	logger := dcontext.GetLogger(ctx)
	allTags, err := ts.All(ctx)
	switch err.(type) {
	case nil:
		break
	case distribution.ErrRepositoryUnknown:
		// This tag store has been initialized but not yet populated
		break
	default:
		return nil, err
	}

	outputChan := make(chan string)

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(ts.lookupConcurrencyFactor)

	go func() {
		for _, tag := range allTags {
			tag := tag // https://go.dev/doc/faq#closures_and_goroutines
			group.Go(func() error {
				tagLinkPathSpec := manifestTagCurrentPathSpec{
					name: ts.repository.Named().Name(),
					tag:  tag,
				}

				tagLinkPath, _ := pathFor(tagLinkPathSpec)
				tagDigest, err := ts.blobStore.readlink(ctx, tagLinkPath)

				if err != nil {
					switch err.(type) {
					// PathNotFoundError shouldn't count as an error
					case storagedriver.PathNotFoundError:
						return nil
					}
					return err
				}

				if tagDigest == desc.Digest {
					outputChan <- tag
				}
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			logger.Errorf("Tag Lookup error: %v", err)
		}
		close(outputChan)
	}()

	tags := make([]string, 0, len(allTags))
	for tag := range outputChan {
		tags = append(tags, tag)
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return tags, nil
}

func (ts *TagStore) ManifestDigests(ctx context.Context, tag string) ([]digest.Digest, error) {
	tagLinkPath := func(name string, dgst digest.Digest) (string, error) {
		return pathFor(manifestTagIndexEntryLinkPathSpec{
			name:     name,
			tag:      tag,
			revision: dgst,
		})
	}
	lbs := &linkedBlobStore{
		blobStore: ts.blobStore,
		blobAccessController: &linkedBlobStatter{
			blobStore:  ts.blobStore,
			repository: ts.repository,
			linkPath:   manifestRevisionLinkPath,
		},
		repository: ts.repository,
		ctx:        ctx,
		linkPath:   tagLinkPath,
		linkDirectoryPathSpec: manifestTagIndexPathSpec{
			name: ts.repository.Named().Name(),
			tag:  tag,
		},
	}
	var dgsts []digest.Digest
	err := lbs.Enumerate(ctx, func(dgst digest.Digest) error {
		dgsts = append(dgsts, dgst)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dgsts, nil
}
