// Package objectstore is where the sender's bytes live: uploaded files, rendered frames,
// and the acknowledgement channel it shares with the receiver.
//
// It is an interface with two implementations because the two deployments it serves are
// genuinely different. A single-machine installation wants a directory — no extra service
// to run, and the shared volume the receiver reads is the same directory. A clustered one
// needs object storage, because several sender instances cannot share a local disk.
// Neither is a fallback for the other.
//
// Both are held to the same conformance suite, which is the only way an interface with two
// implementations stays honest: a test written against the filesystem backend alone would
// quietly come to depend on filesystem behaviour that S3 does not have.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Errors returned by every backend.
var (
	// ErrNotFound means no object has that key.
	ErrNotFound = errors.New("objectstore: no such object")

	// ErrBadKey means the key is not one this store will accept. See CheckKey.
	ErrBadKey = errors.New("objectstore: invalid key")
)

// Object describes a stored object.
type Object struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Store is the interface every backend implements.
//
// Keys are slash-separated paths without a leading slash, the same shape in both backends,
// so a deployment can move between them without rewriting what it stored.
type Store interface {
	// Name is the backend's configuration name, for logs and diagnostics.
	Name() string

	// Put writes an object, replacing whatever was there.
	//
	// The write is atomic in both backends: a reader either sees the previous object or the
	// complete new one, never a partial write. That is not a convenience — the sender writes
	// frames into a directory the receiver is simultaneously reading, and a half-written
	// frame would be captured, fail to decode, and be counted as an optical error rather than
	// the local mistake it was.
	Put(ctx context.Context, key string, r io.Reader) error

	// Get opens an object for reading. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Stat returns an object's metadata without reading it.
	Stat(ctx context.Context, key string) (Object, error)

	// Exists reports whether an object is there.
	Exists(ctx context.Context, key string) (bool, error)

	// Delete removes an object. Deleting something that is not there is not an error, so
	// cleanup is idempotent and a retried job does not fail on its second pass.
	Delete(ctx context.Context, key string) error

	// List returns the objects under a prefix, in key order.
	List(ctx context.Context, prefix string) ([]Object, error)

	// Close releases whatever the backend holds.
	Close() error
}

// MaxKeyLength bounds a key. It is generous enough for the deepest path the sender builds
// and short enough to stay inside the limits of both a filesystem and S3.
const MaxKeyLength = 512

// CheckKey validates a key.
//
// This is the security boundary of the package, and the reason it is one function rather
// than a habit each backend follows. Keys are built from things that came from outside: an
// uploaded filename, a transmission identifier from a request, a session name. The
// filesystem backend turns a key into a path, so a key containing a parent reference would
// write outside the store's root — and the sender runs as a service with write access to
// rather more than its own directory.
//
// Rejecting rather than sanitising is deliberate. Sanitising a hostile key produces a
// different key that is accepted, which means two distinct inputs can collide on one
// object; refusing it means the caller that built it wrongly hears about it.
func CheckKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: empty", ErrBadKey)
	case len(key) > MaxKeyLength:
		return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", ErrBadKey, len(key), MaxKeyLength)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: not valid UTF-8", ErrBadKey)
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("%w: %q is absolute", ErrBadKey, key)
	case strings.HasSuffix(key, "/"):
		return fmt.Errorf("%w: %q names a directory", ErrBadKey, key)
	case strings.Contains(key, `\`):
		// Refused because a backslash is a separator on some filesystems and an ordinary
		// character on others, so a key containing one means different things in different
		// deployments.
		return fmt.Errorf("%w: %q contains a backslash", ErrBadKey, key)
	case strings.Contains(key, "//"):
		return fmt.Errorf("%w: %q has an empty path element", ErrBadKey, key)
	}

	for _, r := range key {
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("%w: %q contains a control character", ErrBadKey, key)
		}
	}

	for _, element := range strings.Split(key, "/") {
		if element == "." || element == ".." {
			return fmt.Errorf("%w: %q contains %q", ErrBadKey, key, element)
		}
	}

	// And the belt-and-braces check: whatever the elements looked like individually, the
	// cleaned path must be the key itself. Anything else means the key had a way of
	// referring to somewhere other than where it appears to.
	if cleaned := path.Clean(key); cleaned != key {
		return fmt.Errorf("%w: %q is not in canonical form (%q)", ErrBadKey, key, cleaned)
	}
	return nil
}

// Key joins path elements into a validated key.
//
// Each element is checked *before* being joined, and that ordering is the whole point.
// path.Join cleans as it joins, so joining "files" with "../frames/000001.png" produces
// "frames/000001.png" — a perfectly canonical key that CheckKey would accept, in a namespace
// the caller did not intend. Checking the result cannot catch it, because by then the escape
// has already been resolved away.
//
// It matters because elements come from outside: an uploaded filename, a name from a request
// body. An upload able to choose "../frames/000001.png" could overwrite a rendered frame, or
// an acknowledgement, without ever leaving the store's root.
func Key(elements ...string) (string, error) {
	for _, element := range elements {
		switch {
		case element == "":
			return "", fmt.Errorf("%w: an empty path element", ErrBadKey)
		case element == "." || element == "..":
			return "", fmt.Errorf("%w: %q is a directory reference", ErrBadKey, element)
		case strings.ContainsAny(element, `/\`):
			return "", fmt.Errorf("%w: %q spans more than one path element", ErrBadKey, element)
		}
	}

	joined := path.Join(elements...)
	if err := CheckKey(joined); err != nil {
		return "", err
	}
	return joined, nil
}

// PutBytes writes a byte slice.
func PutBytes(ctx context.Context, s Store, key string, data []byte) error {
	return s.Put(ctx, key, strings.NewReader(string(data)))
}

// GetBytes reads a whole object.
//
// limit bounds what will be read, because the caller is often reading something it did not
// write — a stored frame, an acknowledgement file — and an object of unexpected size should
// be refused rather than allocated.
func GetBytes(ctx context.Context, s Store, key string, limit int64) ([]byte, error) {
	r, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	if limit <= 0 {
		limit = 64 << 20
	}
	// One byte past the limit, so exceeding it is detectable rather than silently truncating.
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("objectstore: %s is larger than the %d bytes allowed", key, limit)
	}
	return data, nil
}
