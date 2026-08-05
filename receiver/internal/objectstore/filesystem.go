package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Filesystem stores objects as files under a root directory.
//
// It is the default backend, and for a single-machine installation it is also the right one:
// the directory the sender writes frames into is the directory the receiver reads them from,
// so the optical channel needs no service between them.
type Filesystem struct {
	root string
}

// NewFilesystem returns a store rooted at dir, creating it if necessary.
//
// The root is resolved to an absolute path with symlinks followed, and every key is checked
// against it afterwards. Resolving once at construction rather than on each operation means
// a symlink swapped in later cannot move the root out from under a write already in
// progress.
func NewFilesystem(dir string) (*Filesystem, error) {
	if dir == "" {
		return nil, errors.New("objectstore: the filesystem backend needs a root directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	return &Filesystem{root: absolute}, nil
}

// Name returns the backend's configuration name.
func (f *Filesystem) Name() string { return "filesystem" }

// Root is the directory objects live under. The display sink and the acknowledgement watcher
// need it, since they work in paths rather than keys.
func (f *Filesystem) Root() string { return f.root }

// path turns a key into an absolute path, refusing anything that would leave the root.
//
// The containment check is repeated here even though CheckKey has already rejected parent
// references, because the two catch different things: CheckKey rejects a key that *says*
// it escapes, and this rejects one that escapes through the filesystem — a component that
// is a symlink pointing elsewhere. Only the second survives being written to disk.
func (f *Filesystem) path(key string) (string, error) {
	if err := CheckKey(key); err != nil {
		return "", err
	}
	full := filepath.Join(f.root, filepath.FromSlash(key))
	if full != f.root && !strings.HasPrefix(full, f.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q resolves outside the store", ErrBadKey, key)
	}
	return full, nil
}

// Put writes an object atomically: to a temporary file in the same directory, then a rename.
//
// Same directory matters, because rename is only atomic within a filesystem. Writing to the
// system temporary directory and renaming across would silently degrade to a copy, and the
// receiver reading the destination would sometimes capture a half-written frame.
func (f *Filesystem) Put(ctx context.Context, key string, r io.Reader) error {
	full, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return fmt.Errorf("objectstore: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".tmp*")
	if err != nil {
		return fmt.Errorf("objectstore: %w", err)
	}
	tmpName := tmp.Name()
	// Nothing else knows about the temporary file, so on any failure it is removed rather
	// than left for somebody to wonder about.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("objectstore: writing %s: %w", key, err)
	}
	// Flushed before the rename, so the object is durable once it is visible. Without it a
	// crash could leave a name pointing at an empty file — worse than the name not existing,
	// because it looks like a successful write.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("objectstore: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("objectstore: %w", err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("objectstore: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("objectstore: %w", err)
	}
	return nil
}

// Get opens an object.
func (f *Filesystem) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := f.path(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	return file, nil
}

// Stat returns an object's metadata.
func (f *Filesystem) Stat(ctx context.Context, key string) (Object, error) {
	full, err := f.path(key)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return Object{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return Object{}, fmt.Errorf("objectstore: %w", err)
	}
	if info.IsDir() {
		// A directory is not an object, and reporting one as though it were would let a
		// caller believe a key exists that it can never read.
		return Object{}, fmt.Errorf("%w: %s is a directory", ErrNotFound, key)
	}
	return Object{Key: key, Size: info.Size(), Modified: info.ModTime()}, nil
}

// Exists reports whether an object is there.
func (f *Filesystem) Exists(ctx context.Context, key string) (bool, error) {
	_, err := f.Stat(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes an object, and is not an error if it was already gone.
func (f *Filesystem) Delete(ctx context.Context, key string) error {
	full, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("objectstore: %w", err)
	}
	return nil
}

// List returns the objects under a prefix, in key order.
//
// The prefix is a key prefix rather than a directory, matching how S3 behaves, so the two
// backends answer the same question. Temporary files are skipped: a Put in progress is not
// an object yet, and a caller listing frames to display must not pick one up.
func (f *Filesystem) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object

	err := filepath.WalkDir(f.root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") && strings.Contains(d.Name(), ".tmp") {
			return nil
		}

		relative, err := filepath.Rel(f.root, full)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished between the walk and the stat is not an error: the store is
			// live, and something else may legitimately have deleted it.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Close releases nothing, since the filesystem backend holds nothing open.
func (f *Filesystem) Close() error { return nil }
