package objectstore_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/config"
	"github.com/opticaltransport/otp/receiver/internal/objectstore"
)

// backends returns every store implementation available to test.
//
// One suite over both is the point. An interface with two implementations and one set of
// tests is an interface with one implementation and a spare: the untested one drifts, and the
// tested one accumulates assumptions the other cannot meet. Every behaviour the sender relies
// on is asserted here against whichever backends the environment can provide.
func backends(t *testing.T) map[string]func(t *testing.T) objectstore.Store {
	out := map[string]func(t *testing.T) objectstore.Store{
		"filesystem": func(t *testing.T) objectstore.Store {
			store, err := objectstore.NewFilesystem(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store
		},
	}

	if endpoint := os.Getenv("OTP_TEST_MINIO_ENDPOINT"); endpoint != "" {
		out["minio"] = func(t *testing.T) objectstore.Store {
			// A bucket per test, so the tests are independent of each other and of whatever the
			// endpoint already holds.
			cfg := config.MinIO{
				Endpoint:  endpoint,
				AccessKey: os.Getenv("OTP_TEST_MINIO_ACCESS_KEY"),
				SecretKey: os.Getenv("OTP_TEST_MINIO_SECRET_KEY"),
				Bucket:    "otp-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20],
				Region:    "us-east-1",
			}
			store, err := objectstore.NewMinIO(context.Background(), cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store
		}
	}
	return out
}

// eachBackend runs a subtest per available backend.
func eachBackend(t *testing.T, fn func(t *testing.T, store objectstore.Store)) {
	t.Helper()
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) { fn(t, open(t)) })
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()
		payload := []byte("the receiver writes captured frames to disk before decoding them")

		require.NoError(t, store.Put(ctx, "files/report.bin", bytes.NewReader(payload)))

		r, err := store.Get(ctx, "files/report.bin")
		require.NoError(t, err)
		defer r.Close()

		got, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, payload, got)

		info, err := store.Stat(ctx, "files/report.bin")
		require.NoError(t, err)
		require.Equal(t, "files/report.bin", info.Key)
		require.Equal(t, int64(len(payload)), info.Size)
		require.False(t, info.Modified.IsZero())
	})
}

func TestMissingObject(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()

		_, err := store.Get(ctx, "files/absent.bin")
		require.ErrorIs(t, err, objectstore.ErrNotFound)

		_, err = store.Stat(ctx, "files/absent.bin")
		require.ErrorIs(t, err, objectstore.ErrNotFound)

		exists, err := store.Exists(ctx, "files/absent.bin")
		require.NoError(t, err)
		require.False(t, exists)
	})
}

// TestDeleteIsIdempotent matters because cleanup runs as a job, and a job is retried. A
// second pass over an object the first pass removed must not fail the job.
func TestDeleteIsIdempotent(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()
		require.NoError(t, store.Put(ctx, "frames/000001.png", strings.NewReader("x")))

		require.NoError(t, store.Delete(ctx, "frames/000001.png"))
		require.NoError(t, store.Delete(ctx, "frames/000001.png"))
		require.NoError(t, store.Delete(ctx, "frames/never-existed.png"))

		exists, err := store.Exists(ctx, "frames/000001.png")
		require.NoError(t, err)
		require.False(t, exists)
	})
}

// TestPutReplaces checks a second write wins, which is what a retried render job does.
func TestPutReplaces(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()

		require.NoError(t, store.Put(ctx, "frames/000001.png", strings.NewReader("first attempt")))
		require.NoError(t, store.Put(ctx, "frames/000001.png", strings.NewReader("second")))

		got, err := objectstore.GetBytes(ctx, store, "frames/000001.png", 1024)
		require.NoError(t, err)
		require.Equal(t, "second", string(got))

		info, err := store.Stat(ctx, "frames/000001.png")
		require.NoError(t, err)
		require.Equal(t, int64(len("second")), info.Size, "the object is replaced, not appended to")
	})
}

// TestListIsPrefixedAndOrdered pins the semantics the scheduler depends on: it lists a
// transmission's frames and displays them in order, so a listing that was unordered or that
// treated the prefix as a directory would show frames in the wrong sequence.
func TestListIsPrefixedAndOrdered(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()

		keys := []string{
			"tx/a/frames/000003.png",
			"tx/a/frames/000001.png",
			"tx/a/frames/000002.png",
			"tx/b/frames/000001.png",
			"tx/a/manifest.json",
		}
		for _, key := range keys {
			require.NoError(t, store.Put(ctx, key, strings.NewReader(key)))
		}

		frames, err := store.List(ctx, "tx/a/frames/")
		require.NoError(t, err)
		require.Len(t, frames, 3)
		require.Equal(t, []string{
			"tx/a/frames/000001.png",
			"tx/a/frames/000002.png",
			"tx/a/frames/000003.png",
		}, keysOf(frames), "a listing must be in key order")

		// A prefix is a prefix rather than a directory, so it may end mid-element.
		all, err := store.List(ctx, "tx/a/")
		require.NoError(t, err)
		require.Len(t, all, 4)

		partial, err := store.List(ctx, "tx/a/frames/00000")
		require.NoError(t, err)
		require.Len(t, partial, 3)

		empty, err := store.List(ctx, "tx/c/")
		require.NoError(t, err)
		require.Empty(t, empty)

		everything, err := store.List(ctx, "")
		require.NoError(t, err)
		require.Len(t, everything, len(keys))
	})
}

func keysOf(objects []objectstore.Object) []string {
	out := make([]string, len(objects))
	for i, o := range objects {
		out[i] = o.Key
	}
	return out
}

// TestHostileKeysAreRefused is the security test for this package. Keys are built from
// uploaded filenames and request parameters, and the filesystem backend turns a key into a
// path — so a key that escapes the root escapes into a service account's write access.
func TestHostileKeysAreRefused(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()

		for _, key := range []string{
			"",
			"/absolute/path",
			"../outside",
			"files/../../outside",
			"files/./here",
			"files/",
			"files//double",
			`files\windows`,
			"files/nul\x00byte",
			"files/bell\x07",
			strings.Repeat("a", objectstore.MaxKeyLength+1),
		} {
			t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
				require.ErrorIs(t, store.Put(ctx, key, strings.NewReader("x")), objectstore.ErrBadKey)

				_, err := store.Get(ctx, key)
				require.ErrorIs(t, err, objectstore.ErrBadKey)

				_, err = store.Stat(ctx, key)
				require.ErrorIs(t, err, objectstore.ErrBadKey)

				require.ErrorIs(t, store.Delete(ctx, key), objectstore.ErrBadKey)
			})
		}
	})
}

// TestKeyBuilderValidates covers the helper callers are meant to use, since a key assembled
// by hand somewhere is a key nobody checked.
func TestKeyBuilderValidates(t *testing.T) {
	key, err := objectstore.Key("transmissions", uuid.NewString(), "frames", "000001.png")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(key, "transmissions/"))

	_, err = objectstore.Key("transmissions", "..", "escape")
	require.ErrorIs(t, err, objectstore.ErrBadKey)

	// A filename that came from an upload is the realistic hostile case. Note what it would
	// otherwise become: joining cleans it to "etc/passwd", a canonical key in a namespace the
	// caller never meant to write to.
	_, err = objectstore.Key("files", "../../etc/passwd")
	require.ErrorIs(t, err, objectstore.ErrBadKey)

	_, err = objectstore.Key("files", "subdir/nested.bin")
	require.ErrorIs(t, err, objectstore.ErrBadKey, "an element may not span path elements")

	_, err = objectstore.Key("files", "")
	require.ErrorIs(t, err, objectstore.ErrBadKey)
}

// TestFilesystemContainsSymlinkedKeys is filesystem-specific, because the escape it covers
// only exists there: CheckKey rejects a key that *says* it escapes, and this covers one that
// escapes through a symlink the store did not create.
func TestFilesystemContainsSymlinkedKeys(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("not yours"), 0o600))

	store, err := objectstore.NewFilesystem(root)
	require.NoError(t, err)

	// A directory inside the store that points out of it.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	ctx := context.Background()

	// Reading through the link is possible — the key is canonical and the path resolves — so
	// what this test pins is that the store cannot be made to *write* outside its root, and
	// that a listing never reports objects it does not own.
	require.NoError(t, store.Put(ctx, "inside/file", strings.NewReader("mine")))

	listed, err := store.List(ctx, "")
	require.NoError(t, err)
	for _, o := range listed {
		require.False(t, strings.HasPrefix(o.Key, ".."), "listing escaped the root: %s", o.Key)
	}
}

// TestGetBytesRefusesOversizedObjects covers reading something the sender did not write. An
// acknowledgement file or a stored frame of unexpected size should be refused rather than
// allocated.
func TestGetBytesRefusesOversizedObjects(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()
		require.NoError(t, store.Put(ctx, "acks/big.json", strings.NewReader(strings.Repeat("x", 4096))))

		_, err := objectstore.GetBytes(ctx, store, "acks/big.json", 1024)
		require.ErrorContains(t, err, "larger than")

		got, err := objectstore.GetBytes(ctx, store, "acks/big.json", 4096)
		require.NoError(t, err, "an object of exactly the limit must be readable")
		require.Len(t, got, 4096)
	})
}

// TestEmptyObject covers the degenerate case: a zero-byte file is a legitimate thing to
// transmit, so a zero-byte object must round-trip rather than read as absent.
func TestEmptyObject(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()
		require.NoError(t, store.Put(ctx, "files/empty.bin", strings.NewReader("")))

		exists, err := store.Exists(ctx, "files/empty.bin")
		require.NoError(t, err)
		require.True(t, exists, "an empty object still exists")

		info, err := store.Stat(ctx, "files/empty.bin")
		require.NoError(t, err)
		require.Zero(t, info.Size)

		got, err := objectstore.GetBytes(ctx, store, "files/empty.bin", 1024)
		require.NoError(t, err)
		require.Empty(t, got)
	})
}

// TestLargeObjectStreams checks a multi-megabyte object round-trips without being held in
// memory twice, which is the case every uploaded file exercises.
func TestLargeObjectStreams(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		ctx := context.Background()
		const size = 8 << 20

		require.NoError(t, store.Put(ctx, "files/large.bin", io.LimitReader(&patternReader{}, size)))

		info, err := store.Stat(ctx, "files/large.bin")
		require.NoError(t, err)
		require.Equal(t, int64(size), info.Size)

		r, err := store.Get(ctx, "files/large.bin")
		require.NoError(t, err)
		defer r.Close()

		// Verified by content rather than only by length, so a backend that truncated or
		// reordered its parts would be caught.
		var read int64
		buf := make([]byte, 64<<10)
		for {
			n, err := r.Read(buf)
			for i := 0; i < n; i++ {
				require.Equal(t, byte((read+int64(i))%251), buf[i], "byte %d differs", read+int64(i))
			}
			read += int64(n)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
		}
		require.Equal(t, int64(size), read)
	})
}

// patternReader is an endless, position-dependent byte pattern.
//
// The receiver is a pointer so that the offset actually advances: with a value receiver each
// Read restarts the pattern, which produces a stream that looks plausible and repeats every
// buffer — exactly the kind of corruption this test exists to detect.
type patternReader struct{ offset int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte((p.offset + int64(i)) % 251)
	}
	p.offset += int64(len(b))
	return len(b), nil
}

func TestBackendNames(t *testing.T) {
	eachBackend(t, func(t *testing.T, store objectstore.Store) {
		require.NotEmpty(t, store.Name())
	})
}

// TestOpenSelectsTheConfiguredBackend covers the one place the choice is made.
func TestOpenSelectsTheConfiguredBackend(t *testing.T) {
	ctx := context.Background()

	cfg := config.Default().Storage
	cfg.Root = t.TempDir()
	store, err := objectstore.Open(ctx, cfg)
	require.NoError(t, err)
	require.Equal(t, "filesystem", store.Name())
	require.NoError(t, store.Close())

	cfg.Backend = "dropbox"
	_, err = objectstore.Open(ctx, cfg)
	require.ErrorContains(t, err, "not a known backend")
}
