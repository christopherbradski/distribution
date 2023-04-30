package schema2

import (
	"context"
	"errors"

	"github.com/distribution/distribution/v3"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
)

// builder is a type for constructing manifests.
type builder struct {
	// bs is a BlobService used to publish the configuration blob.
	bs distribution.BlobService

	// configMediaType is media type used to describe configuration
	configMediaType string

	// configJSON references
	configJSON []byte

	// dependencies is a list of descriptors that gets built by successive
	// calls to AppendReference. In case of image configuration these are layers.
	dependencies []distribution.Descriptor
}

// NewManifestBuilder is used to build new manifests for the current schema
// version. It takes a BlobService so it can publish the configuration blob
// as part of the Build process.
func NewManifestBuilder(bs distribution.BlobService, configMediaType string, configJSON []byte) distribution.ManifestBuilder {
	mb := &builder{
		bs:              bs,
		configMediaType: configMediaType,
		configJSON:      make([]byte, len(configJSON)),
	}
	copy(mb.configJSON, configJSON)

	return mb
}

// Build produces a final manifest from the given references.
func (mb *builder) Build(ctx context.Context) (distribution.Manifest, error) {
	m := Manifest{
		Versioned: specs.Versioned{SchemaVersion: defaultSchemaVersion},
		MediaType: defaultMediaType,
		Layers:    make([]distribution.Descriptor, len(mb.dependencies)),
	}
	copy(m.Layers, mb.dependencies)

	configDigest := digest.FromBytes(mb.configJSON)

	var err error
	m.Config, err = mb.bs.Stat(ctx, configDigest)
	switch err {
	case nil:
		// Override MediaType, since Put always replaces the specified media
		// type with application/octet-stream in the descriptor it returns.
		m.Config.MediaType = mb.configMediaType
		return FromStruct(m)
	case distribution.ErrBlobUnknown:
		// nop
	default:
		return nil, err
	}

	// Add config to the blob store
	m.Config, err = mb.bs.Put(ctx, mb.configMediaType, mb.configJSON)
	// Override MediaType, since Put always replaces the specified media
	// type with application/octet-stream in the descriptor it returns.
	m.Config.MediaType = mb.configMediaType
	if err != nil {
		return nil, err
	}

	return FromStruct(m)
}

// AppendReference adds a reference to the current ManifestBuilder.
//
// The reference must be either a [distribution.Descriptor] or a
// [distribution.Describable].
func (mb *builder) AppendReference(d any) error {
	var descriptor distribution.Descriptor
	if dt, ok := d.(distribution.Descriptor); ok {
		descriptor = dt
	} else if dt, ok := d.(distribution.Describable); ok {
		descriptor = dt.Descriptor()
	} else {
		return errors.New("invalid type for reference: should be either a Descriptor or a Describable")
	}

	mb.dependencies = append(mb.dependencies, descriptor)
	return nil
}

// References returns the current references added to this builder.
func (mb *builder) References() []distribution.Descriptor {
	return mb.dependencies
}
