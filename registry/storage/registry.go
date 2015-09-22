package storage

import (
	"fmt"
	"github.com/docker/distribution"
	"github.com/docker/distribution/context"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/storage/cache"
	storagedriver "github.com/docker/distribution/registry/storage/driver"
)

// registry is the top-level implementation of Registry for use in the storage
// package. All instances should descend from this object.
type registry struct {
	blobStore                   *blobStore
	blobServer                  *blobServer
	statter                     *blobStatter // global statter service.
	blobDescriptorCacheProvider cache.BlobDescriptorCacheProvider
	deleteEnabled               bool
	resumableDigestEnabled      bool
}

// RegistryOption is the type used for functional options for NewRegistry.
type RegistryOption func(*registry) error

// EnableRedirect is a functional option for NewRegistry. It causes the backend
// blob server to attempt using (StorageDriver).URLFor to serve all blobs.
func EnableRedirect(registry *registry) error {
	registry.blobServer.redirect = true
	return nil
}

// EnableDelete is a functional option for NewRegistry. It enables deletion on
// the registry.
func EnableDelete(registry *registry) error {
	registry.blobStore.deleteEnabled = true
	registry.deleteEnabled = true
	return nil
}

// DisableDigestResumption is a functional option for NewRegistry. It should be
// used if the registry is acting as a caching proxy.
func DisableDigestResumption(registry *registry) error {
	registry.resumableDigestEnabled = false
	return nil
}

// RemoveParentsOnDelete is a functional option for NewRegistry. It causes
// parent directory of blob's data or link to be deleted as well during Delete.
// It should be used only with storage drivers providing strong consistency.
// Must be used together with `EnableDelete`.
func RemoveParentsOnDelete(registry *registry) error {
	registry.blobStore.removeParentsOnDelete = true
	return nil
}

// BlobDescriptorCacheProvider returns a functional option for
// NewRegistry. It creates a cached blob statter for use by the
// registry.
func BlobDescriptorCacheProvider(blobDescriptorCacheProvider cache.BlobDescriptorCacheProvider) RegistryOption {
	// TODO(aaronl): The duplication of statter across several objects is
	// ugly, and prevents us from using interface types in the registry
	// struct. Ideally, blobStore and blobServer should be lazily
	// initialized, and use the current value of
	// blobDescriptorCacheProvider.
	return func(registry *registry) error {
		if blobDescriptorCacheProvider != nil {
			statter := cache.NewCachedBlobStatter(blobDescriptorCacheProvider, registry.statter)
			registry.blobStore.statter = statter
			registry.blobServer.statter = statter
			registry.blobDescriptorCacheProvider = blobDescriptorCacheProvider
		}
		return nil
	}
}

// NewRegistry creates a new registry instance from the provided driver. The
// resulting registry may be shared by multiple goroutines but is cheap to
// allocate. If the Redirect option is specified, the backend blob server will
// attempt to use (StorageDriver).URLFor to serve all blobs.
func NewRegistry(ctx context.Context, driver storagedriver.StorageDriver, options ...RegistryOption) (distribution.Namespace, error) {
	// create global statter
	statter := &blobStatter{
		driver: driver,
	}

	bs := &blobStore{
		driver:  driver,
		statter: statter,
	}

	registry := &registry{
		blobStore: bs,
		blobServer: &blobServer{
			driver:  driver,
			statter: statter,
			pathFn:  bs.path,
		},
		statter:                statter,
		resumableDigestEnabled: true,
	}

	for _, option := range options {
		if err := option(registry); err != nil {
			return nil, err
		}
	}

	return registry, nil
}

// Scope returns the namespace scope for a registry. The registry
// will only serve repositories contained within this scope.
func (reg *registry) Scope() distribution.Scope {
	return distribution.GlobalScope
}

// Repository returns an instance of the repository tied to the registry.
// Instances should not be shared between goroutines but are cheap to
// allocate. In general, they should be request scoped.
func (reg *registry) Repository(ctx context.Context, canonicalName reference.Named) (distribution.Repository, error) {
	var descriptorCache distribution.BlobDescriptorService
	if reg.blobDescriptorCacheProvider != nil {
		var err error
		descriptorCache, err = reg.blobDescriptorCacheProvider.RepositoryScoped(canonicalName.Name())
		if err != nil {
			return nil, err
		}
	}

	return &repository{
		ctx:             ctx,
		registry:        reg,
		name:            canonicalName,
		descriptorCache: descriptorCache,
	}, nil
}

// Blobs returns an instance of the BlobServer for registry's blob access.
func (reg *registry) Blobs() distribution.BlobService {
	return reg.blobStore
}

// RegistryBlobEnumerator returns an instance of BlobEnumerator for given registry object.
func RegistryBlobEnumerator(ns distribution.Namespace) (distribution.BlobEnumerator, error) {
	reg, ok := ns.(*registry)
	if !ok {
		return nil, fmt.Errorf("cannot instantiate BlobEnumerator with given namespace object (%T)", ns)
	}
	return reg.blobStore, nil
}

// RegistryBlobDeleter returns an instance of BlobDeleter for given registry object.
func RegistryBlobDeleter(ns distribution.Namespace) (distribution.BlobDeleter, error) {
	reg, ok := ns.(*registry)
	if !ok {
		return nil, fmt.Errorf("cannot instantiate BlobDeleter with given namespace object (%T)", ns)
	}
	return reg.blobStore, nil
}

// repository provides name-scoped access to various services.
type repository struct {
	*registry
	ctx             context.Context
	name            reference.Named
	descriptorCache distribution.BlobDescriptorService
}

// Name returns the name of the repository.
func (repo *repository) Name() reference.Named {
	return repo.name
}

func (repo *repository) Tags(ctx context.Context) distribution.TagService {
	tags := &tagStore{
		repository: repo,
		blobStore:  repo.registry.blobStore,
	}

	return tags
}

// Manifests returns an instance of ManifestService. Instantiation is cheap and
// may be context sensitive in the future. The instance should be used similar
// to a request local.
func (repo *repository) Manifests(ctx context.Context, options ...distribution.ManifestServiceOption) (distribution.ManifestService, error) {
	manifestLinkPathFns := []linkPathFunc{
		// NOTE(stevvooe): Need to search through multiple locations since
		// 2.1.0 unintentionally linked into  _layers.
		manifestRevisionLinkPath,
		blobLinkPath,
	}
	manifestRootPathFns := []blobsRootPathFunc{
		manifestRevisionsPath,
		blobsRootPath,
	}

	blobStore := &linkedBlobStore{
		ctx:           ctx,
		blobStore:     repo.blobStore,
		repository:    repo,
		deleteEnabled: repo.registry.deleteEnabled,
		blobAccessController: &linkedBlobStatter{
			blobStore:             repo.blobStore,
			repository:            repo,
			linkPathFns:           manifestLinkPathFns,
			removeParentsOnDelete: repo.registry.blobStore.removeParentsOnDelete,
		},

		// TODO(stevvooe): linkPath limits this blob store to only
		// manifests. This instance cannot be used for blob checks.
		linkPathFns:      manifestLinkPathFns,
		blobsRootPathFns: manifestRootPathFns,
	}

	ms := &manifestStore{
		ctx:        ctx,
		repository: repo,
		blobStore:  blobStore,
		schema1Handler: &signedManifestHandler{
			ctx:        ctx,
			repository: repo,
			blobStore:  blobStore,
			signatures: &signatureStore{
				ctx:        ctx,
				repository: repo,
				blobStore:  repo.blobStore,
			},
		},
		schema2Handler: &schema2ManifestHandler{
			ctx:        ctx,
			repository: repo,
			blobStore:  blobStore,
		},
		manifestListHandler: &manifestListHandler{
			ctx:        ctx,
			repository: repo,
			blobStore:  blobStore,
		},
	}

	// Apply options
	for _, option := range options {
		err := option.Apply(ms)
		if err != nil {
			return nil, err
		}
	}

	return ms, nil
}

// Blobs returns an instance of the BlobStore. Instantiation is cheap and
// may be context sensitive in the future. The instance should be used similar
// to a request local.
func (repo *repository) Blobs(ctx context.Context) distribution.BlobStore {
	var statter distribution.BlobDescriptorService = &linkedBlobStatter{
		blobStore:             repo.blobStore,
		repository:            repo,
		linkPathFns:           []linkPathFunc{blobLinkPath},
		removeParentsOnDelete: repo.registry.blobStore.removeParentsOnDelete,
	}

	if repo.descriptorCache != nil {
		statter = cache.NewCachedBlobStatter(repo.descriptorCache, statter)
	}

	return &linkedBlobStore{
		registry:             repo.registry,
		blobStore:            repo.blobStore,
		blobServer:           repo.blobServer,
		blobAccessController: statter,
		repository:           repo,
		ctx:                  ctx,

		// TODO(stevvooe): linkPath limits this blob store to only layers.
		// This instance cannot be used for manifest checks.
		linkPathFns:            []linkPathFunc{blobLinkPath},
		blobsRootPathFns:       []blobsRootPathFunc{blobsRootPath},
		deleteEnabled:          repo.registry.deleteEnabled,
		resumableDigestEnabled: repo.resumableDigestEnabled,
	}
}
