package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/opticaltransport/otp/receiver/internal/config"
)

// MinIO stores objects in an S3-compatible bucket.
//
// It is the backend for a deployment running more than one sender instance, where a local
// directory cannot be shared. Everything the filesystem backend gets from the operating
// system — atomic replacement, listing, absence of an object — this gets from the object
// store's own semantics, which is why the conformance suite runs against both: the two are
// only interchangeable if they agree, and nothing but a test keeps them agreeing.
type MinIO struct {
	client *minio.Client
	bucket string
}

// NewMinIO connects to an S3-compatible store and ensures the bucket exists.
func NewMinIO(ctx context.Context, cfg config.MinIO) (*MinIO, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("objectstore: the minio backend needs an endpoint and a bucket")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}

	// Reaching the store at startup rather than on the first upload, for the same reason the
	// database connection is verified there: a misconfigured endpoint should fail where
	// somebody is watching, not on the first request an operator makes.
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objectstore: could not reach %s: %w", cfg.Endpoint, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			// Another instance starting at the same moment may have won the race, which is the
			// outcome this wants anyway.
			if exists, checkErr := client.BucketExists(ctx, cfg.Bucket); checkErr != nil || !exists {
				return nil, fmt.Errorf("objectstore: could not create bucket %s: %w", cfg.Bucket, err)
			}
		}
	}

	return &MinIO{client: client, bucket: cfg.Bucket}, nil
}

// Name returns the backend's configuration name.
func (m *MinIO) Name() string { return "minio" }

// Put writes an object.
//
// The size is unknown because callers stream, so it is passed as -1 and the client uploads
// in parts. An S3 put is atomic by construction: the object is not visible until the upload
// completes, so there is no temporary-file dance to do here.
func (m *MinIO) Put(ctx context.Context, key string, r io.Reader) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	_, err := m.client.PutObject(ctx, m.bucket, key, r, -1, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("objectstore: writing %s: %w", key, err)
	}
	return nil
}

// Get opens an object.
//
// The object is stat-ed before being returned, because a MinIO reader defers its request:
// without this, a missing object would surface as a read error later, in whatever code was
// copying bytes, rather than as ErrNotFound here — and callers distinguishing "not there"
// from "went wrong" would be unable to.
func (m *MinIO) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := CheckKey(key); err != nil {
		return nil, err
	}
	if _, err := m.Stat(ctx, key); err != nil {
		return nil, err
	}
	object, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, m.translate(key, err)
	}
	return object, nil
}

// Stat returns an object's metadata.
func (m *MinIO) Stat(ctx context.Context, key string) (Object, error) {
	if err := CheckKey(key); err != nil {
		return Object{}, err
	}
	info, err := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return Object{}, m.translate(key, err)
	}
	return Object{Key: key, Size: info.Size, Modified: info.LastModified}, nil
}

// Exists reports whether an object is there.
func (m *MinIO) Exists(ctx context.Context, key string) (bool, error) {
	_, err := m.Stat(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes an object, and is not an error if it was already gone — which S3 gives for
// free, since a delete of a missing key succeeds there.
func (m *MinIO) Delete(ctx context.Context, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	if err := m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: deleting %s: %w", key, err)
	}
	return nil
}

// List returns the objects under a prefix, in key order.
func (m *MinIO) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for object := range m.client.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{
		Prefix: prefix,
		// Recursive, so the prefix behaves as a key prefix rather than as a directory — the
		// same question the filesystem backend answers.
		Recursive: true,
	}) {
		if object.Err != nil {
			return nil, fmt.Errorf("objectstore: listing %q: %w", prefix, object.Err)
		}
		out = append(out, Object{
			Key:      object.Key,
			Size:     object.Size,
			Modified: object.LastModified,
		})
	}
	// The listing arrives in key order already, so nothing is sorted here; the conformance
	// suite checks the ordering rather than this comment being trusted.
	return out, nil
}

// Close releases nothing the client holds open beyond its idle connections.
func (m *MinIO) Close() error { return nil }

// translate turns an S3 error into this package's vocabulary.
func (m *MinIO) translate(key string, err error) error {
	response := minio.ToErrorResponse(err)
	switch response.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return fmt.Errorf("objectstore: %s: %w", key, err)
}

// Open returns the store a configuration selects.
//
// Having one function decide means the choice is made in a single place rather than wherever
// a store happens to be needed, and an unknown backend name is caught here rather than by
// falling back to a default the operator did not ask for.
func Open(ctx context.Context, cfg config.Storage) (Store, error) {
	switch cfg.Backend {
	case "filesystem":
		return NewFilesystem(cfg.Root)
	case "minio":
		return NewMinIO(ctx, cfg.MinIO)
	default:
		return nil, fmt.Errorf("objectstore: %q is not a known backend", cfg.Backend)
	}
}
