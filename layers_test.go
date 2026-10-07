package storage

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"unsafe"

	graphdriver "github.com/containers/storage/drivers"
	"github.com/containers/storage/drivers/vfs"
	"github.com/containers/storage/internal/tempdir"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/directory"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/ioutils"
	"github.com/klauspost/pgzip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tarsplitasm "github.com/vbatts/tar-split/tar/asm"
	tarsplitstorage "github.com/vbatts/tar-split/tar/storage"
)

func TestLayerLocationFromIndex(t *testing.T) {
	tests := []struct {
		index    int
		expected layerLocations
	}{
		{0, 1},
		{1, 2},
		{2, 4},
		{3, 8},
		{4, 16},
	}
	for _, test := range tests {
		result := layerLocationFromIndex(test.index)
		assert.Equal(t, test.expected, result)
	}
}

func TestLayerLocationFromIndexAndToIndex(t *testing.T) {
	var l layerLocations
	for i := range int(unsafe.Sizeof(l) * 8) {
		location := layerLocationFromIndex(i)
		index := indexFromLayerLocation(location)
		require.Equal(t, i, index)
	}
}

// countingProtoDriver wraps a real ProtoDriver but deliberately does NOT implement
// graphdriver.DiffGetterDriver: this is the btrfs/zfs driver shape, for which layerStore.Diff
// has to take a read-only driver.Get() reference for tar-stream reconstruction.
// It records every Get/Put so that reference balancing can be checked.
type countingProtoDriver struct {
	inner graphdriver.ProtoDriver

	mu       sync.Mutex
	gets     map[string]int
	puts     map[string]int
	failNext map[string]int // ID -> number of the next Get() calls to fail
}

func newCountingProtoDriver(inner graphdriver.ProtoDriver) *countingProtoDriver {
	return &countingProtoDriver{
		inner:    inner,
		gets:     make(map[string]int),
		puts:     make(map[string]int),
		failNext: make(map[string]int),
	}
}

func (c *countingProtoDriver) String() string { return c.inner.String() }

func (c *countingProtoDriver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	return c.inner.CreateReadWrite(id, parent, opts)
}

func (c *countingProtoDriver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	return c.inner.Create(id, parent, opts)
}

func (c *countingProtoDriver) CreateFromTemplate(id, template string, templateIDMappings *idtools.IDMappings, parent string, parentIDMappings *idtools.IDMappings, opts *graphdriver.CreateOpts, readWrite bool) error {
	return c.inner.CreateFromTemplate(id, template, templateIDMappings, parent, parentIDMappings, opts, readWrite)
}

func (c *countingProtoDriver) Remove(id string) error { return c.inner.Remove(id) }

func (c *countingProtoDriver) DeferredRemove(id string) (tempdir.CleanupTempDirFunc, error) {
	return c.inner.DeferredRemove(id)
}

func (c *countingProtoDriver) GetTempDirRootDirs() []string { return c.inner.GetTempDirRootDirs() }

func (c *countingProtoDriver) Get(id string, options graphdriver.MountOpts) (string, error) {
	c.mu.Lock()
	if c.failNext[id] > 0 {
		c.failNext[id]--
		c.mu.Unlock()
		return "", errors.New("forced Get() failure")
	}
	c.gets[id]++
	c.mu.Unlock()
	dir, err := c.inner.Get(id, options)
	if err != nil {
		c.mu.Lock()
		c.gets[id]--
		c.mu.Unlock()
	}
	return dir, err
}

func (c *countingProtoDriver) Put(id string) error {
	c.mu.Lock()
	c.puts[id]++
	c.mu.Unlock()
	return c.inner.Put(id)
}

func (c *countingProtoDriver) Exists(id string) bool { return c.inner.Exists(id) }

func (c *countingProtoDriver) ListLayers() ([]string, error) { return c.inner.ListLayers() }

func (c *countingProtoDriver) Status() [][2]string { return c.inner.Status() }

func (c *countingProtoDriver) Metadata(id string) (map[string]string, error) {
	return c.inner.Metadata(id)
}

func (c *countingProtoDriver) ReadWriteDiskUsage(id string) (*directory.DiskUsage, error) {
	return c.inner.ReadWriteDiskUsage(id)
}

func (c *countingProtoDriver) Cleanup() error { return c.inner.Cleanup() }

func (c *countingProtoDriver) AdditionalImageStores() []string {
	return c.inner.AdditionalImageStores()
}

func (c *countingProtoDriver) Dedup(args graphdriver.DedupArgs) (graphdriver.DedupResult, error) {
	return c.inner.Dedup(args)
}

func (c *countingProtoDriver) counts(id string) (gets, puts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets[id], c.puts[id]
}

func (c *countingProtoDriver) failOneGet(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext[id] = 1
}

// newLayerStoreForDiffTest creates a layerStore backed by a driver with the btrfs/zfs shape
// (naive diff driver WITHOUT DiffGetter), returning the store, the counting driver and the
// paths of its graph driver home and layer/run directories.
func newLayerStoreForDiffTest(t *testing.T) (*layerStore, *countingProtoDriver, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	graphHome := filepath.Join(dir, "graph")
	runDir := filepath.Join(dir, "run")
	layerDir := filepath.Join(dir, "layers")
	vfsDriver, err := vfs.Init(graphHome, graphdriver.Options{})
	require.NoError(t, err)
	counter := newCountingProtoDriver(vfsDriver)
	driver := graphdriver.NewNaiveDiffDriver(counter, graphdriver.NewNaiveLayerIDMapUpdater(counter))
	// The whole point of this test is the non-DiffGetter path.
	_, ok := driver.(graphdriver.DiffGetterDriver)
	require.False(t, ok, "the test driver must not implement DiffGetterDriver")
	store, err := new(store).newLayerStore(runDir, layerDir, "", driver, false)
	require.NoError(t, err)
	rlstore, ok := store.(*layerStore)
	require.True(t, ok)
	return rlstore, counter, graphHome, runDir, layerDir
}

// newCountingDriver creates another naive (non-DiffGetter) driver backed by a (possibly shared)
// graph driver home, returning it together with its Get/Put counter.
func newCountingDriver(t *testing.T, graphHome string) (*countingProtoDriver, graphdriver.Driver) {
	t.Helper()
	vfsDriver, err := vfs.Init(graphHome, graphdriver.Options{})
	require.NoError(t, err)
	counter := newCountingProtoDriver(vfsDriver)
	driver := graphdriver.NewNaiveDiffDriver(counter, graphdriver.NewNaiveLayerIDMapUpdater(counter))
	_, ok := driver.(graphdriver.DiffGetterDriver)
	require.False(t, ok, "the test driver must not implement DiffGetterDriver")
	return counter, driver
}

// tarWithOneFile returns an uncompressed tar archive containing a single regular file.
func tarWithOneFile(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// tarSplitData disassembles an uncompressed tar archive into the gzipped tar-split metadata
// written by layerStore.applyDiffWithOptions, driving the disassembly to completion here
// (instead of going through the pooled-decompressor apply path) so that setup is deterministic.
func tarSplitData(t *testing.T, tarData []byte) []byte {
	t.Helper()
	var tsdata bytes.Buffer
	compressor := pgzip.NewWriter(&tsdata)
	packer := tarsplitstorage.NewJSONPacker(compressor)
	payload, err := tarsplitasm.NewInputTarStream(bytes.NewReader(tarData), packer, tarsplitstorage.NewDiscardFilePutter())
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, payload)
	require.NoError(t, err)
	require.NoError(t, compressor.Close())
	return tsdata.Bytes()
}

// createDiffTestLayer creates a layer record plus driver directory, then populates it with
// hello.txt and a matching tar-split file, exercising the tar-split Diff reconstruction path.
func createDiffTestLayer(t *testing.T, r *layerStore, id string) []byte {
	t.Helper()
	tarData := tarWithOneFile(t, "hello.txt", "hello\n")
	require.NoError(t, r.startWriting())
	_, _, err := r.create(id, nil, nil, "", nil, &LayerOptions{}, false, nil, nil)
	require.NoError(t, err)
	dir, err := r.Mount(id, graphdriver.MountOpts{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644))
	require.NoError(t, ioutils.AtomicWriteFile(r.tspath(id), tarSplitData(t, tarData), 0o600))
	stillMounted, err := r.unmount(id, false, true)
	require.NoError(t, err)
	require.False(t, stillMounted)
	r.stopWriting()
	return tarData
}

// readTarEntries reads an uncompressed tar stream, returning regular files keyed by name.
func readTarEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	entries, err := tarFileContents(data)
	require.NoError(t, err)
	return entries
}

// tarFileContents parses an uncompressed tar archive and returns regular files keyed by name.
// It does not depend on *testing.T so it is safe to call from worker goroutines.
func tarFileContents(data []byte) (map[string]string, error) {
	entries := make(map[string]string)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg {
			contents, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			entries[header.Name] = string(contents)
		}
	}
	return entries, nil
}

func uncompressedDiffOptions() *DiffOptions {
	compression := archive.Uncompressed
	return &DiffOptions{Compression: &compression}
}

// TestLayerStoreDiffPrivateDriverReference verifies that Diff with a non-DiffGetter driver
// (btrfs/zfs shape) only uses its own driver.Get/Put reference, never layerStore mount records.
func TestLayerStoreDiffPrivateDriverReference(t *testing.T) {
	r, counter, _, _, _ := newLayerStoreForDiffTest(t)
	const id = "diff-layer"
	createDiffTestLayer(t, r, id)
	options := uncompressedDiffOptions()

	// A plain Diff under the read lock produces the expected contents and is fully balanced.
	require.NoError(t, r.startReading())
	rc, err := r.Diff("", id, options)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello\n", readTarEntries(t, data)["hello.txt"])
	require.NoError(t, rc.Close())
	r.stopReading()

	gets, puts := counter.counts(id)
	require.Equal(t, gets, puts, "every Diff driver.Get() must be matched by driver.Put()")

	// Diff must not leave any store-recorded mount (count, mount point or mountpoints.json entry).
	require.NoError(t, r.startReading())
	mountCount, err := r.Mounted(id)
	require.NoError(t, err)
	r.stopReading()
	require.Zero(t, mountCount, "Diff must not modify the layer store mount reference count")
	mountsData, err := os.ReadFile(r.mountspath())
	if err == nil {
		var mounts []layerMountPoint
		require.NoError(t, json.Unmarshal(mountsData, &mounts))
		for _, mount := range mounts {
			require.NotEqual(t, id, mount.ID, "Diff must not record a mount in mountpoints.json")
		}
	} else {
		require.True(t, os.IsNotExist(err))
	}
}

// TestLayerStoreDiffConcurrent verifies that many concurrent Diff invocations (modeling two or
// more processes diffing the same layer) each get a correct archive and only release their own
// driver reference, with balanced Get/Put and no store mount record afterwards.
func TestLayerStoreDiffConcurrent(t *testing.T) {
	r, counter, _, _, _ := newLayerStoreForDiffTest(t)
	const id = "concurrent-diff-layer"
	createDiffTestLayer(t, r, id)
	options := uncompressedDiffOptions()

	// Establish the expected archive contents first.
	require.NoError(t, r.startReading())
	rc, err := r.Diff("", id, options)
	require.NoError(t, err)
	expected, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	r.stopReading()
	baselineGets, baselinePuts := counter.counts(id)

	const goroutines = 16
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := func() error {
				if err := r.startReading(); err != nil {
					return err
				}
				defer r.stopReading()
				rc, err := r.Diff("", id, options)
				if err != nil {
					return err
				}
				data, readErr := io.ReadAll(rc)
				closeErr := rc.Close()
				if readErr != nil {
					return readErr
				}
				if closeErr != nil {
					return closeErr
				}
				if !bytes.Equal(expected, data) {
					return errors.New("concurrent Diff produced a wrong archive")
				}
				return nil
			}()
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	gets, puts := counter.counts(id)
	require.Equal(t, gets, puts, "all concurrent Diff driver references must be released")
	require.Equal(t, baselineGets+goroutines, gets, "each Diff must take exactly one driver.Get() reference")
	require.Equal(t, baselinePuts+goroutines, puts, "each Diff must release exactly one driver.Put() reference")

	require.NoError(t, r.startReading())
	mountCount, err := r.Mounted(id)
	require.NoError(t, err)
	r.stopReading()
	require.Zero(t, mountCount)
}

// TestLayerStoreDiffCancelAndRepeatedClose verifies cancellation (closing the stream early) and
// repeated Close()s release exactly one driver reference.
func TestLayerStoreDiffCancelAndRepeatedClose(t *testing.T) {
	r, counter, _, _, _ := newLayerStoreForDiffTest(t)
	const id = "cancel-diff-layer"
	createDiffTestLayer(t, r, id)
	options := uncompressedDiffOptions()

	require.NoError(t, r.startReading())
	rc, err := r.Diff("", id, options)
	require.NoError(t, err)
	// Read just a few bytes, then cancel the rest by closing early.
	buf := make([]byte, 32)
	n, err := rc.Read(buf)
	require.Greater(t, n, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		require.NoError(t, err)
	}
	require.NoError(t, rc.Close())
	getsOnce, putsOnce := counter.counts(id)
	// A second Close (e.g. both a read-error path and an explicit close firing) must not
	// release anything extra. Other tar handles are not idempotent, so the error is tolerated;
	// what matters is that our driver mount reference is released at most once.
	_ = rc.Close()
	getsTwice, putsTwice := counter.counts(id)
	require.Equal(t, getsOnce, getsTwice)
	require.Equal(t, putsOnce, putsTwice, "Close() must release the driver reference at most once")
	r.stopReading()

	gets, puts := counter.counts(id)
	require.Equal(t, gets, puts, "cancellation must leave driver references balanced")
}

// TestLayerStoreDiffFailureRetriesAndPreservesMounts verifies that a failed Diff leaves no
// driver reference behind, does not lose a pre-existing store mount reference, and that a later
// Diff succeeds ("failure doesn't lose existing references, Diff is retryable").
func TestLayerStoreDiffFailureRetriesAndPreservesMounts(t *testing.T) {
	r, counter, _, _, _ := newLayerStoreForDiffTest(t)
	const id = "retry-diff-layer"
	createDiffTestLayer(t, r, id)
	options := uncompressedDiffOptions()

	// Simulate an unrelated user (e.g. a running container) holding one store-level mount.
	require.NoError(t, r.startWriting())
	mountPoint, err := r.Mount(id, graphdriver.MountOpts{})
	require.NoError(t, err)
	require.NotEmpty(t, mountPoint)
	count, err := r.Mounted(id)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	r.stopWriting()

	// Make exactly the next driver.Get() fail, so the Diff must fail.
	counter.failOneGet(id)
	getsBefore, putsBefore := counter.counts(id)
	require.NoError(t, r.startReading())
	_, err = r.Diff("", id, options)
	require.Error(t, err)
	r.stopReading()
	getsAfter, putsAfter := counter.counts(id)
	require.Equal(t, getsBefore, getsAfter, "a failed Get() must not be counted as a reference")
	require.Equal(t, putsBefore, putsAfter, "a failed Diff must not Put anything")

	// The pre-existing store-level mount reference is untouched and still counted once.
	require.NoError(t, r.startReading())
	count, err = r.Mounted(id)
	require.NoError(t, err)
	r.stopReading()
	require.Equal(t, 1, count, "a failed Diff must not lose the existing store mount reference")

	// A subsequent Diff succeeds and returns correct data.
	require.NoError(t, r.startReading())
	rc, err := r.Diff("", id, options)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello\n", readTarEntries(t, data)["hello.txt"])
	require.NoError(t, rc.Close())
	r.stopReading()
	gets, puts := counter.counts(id)
	// Exactly one reference is still outstanding: the pre-existing store mount, not the Diff.
	require.Equal(t, puts+1, gets, "the retried Diff must have released its own driver reference")

	// The pre-existing mount is still released exactly once by its own owner.
	require.NoError(t, r.startWriting())
	count, err = r.Mounted(id)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	stillMounted, err := r.unmount(id, false, false)
	require.NoError(t, err)
	require.False(t, stillMounted)
	count, err = r.Mounted(id)
	require.NoError(t, err)
	require.Zero(t, count)
	r.stopWriting()

	gets, puts = counter.counts(id)
	require.Equal(t, gets, puts, "all driver references must be balanced at the end")
}

// TestLayerStoreDiffOnReadOnlyStore verifies Diff works on a read-only/additional layer store
// (whose mountsLockfile is nil) with a btrfs/zfs-shaped driver: the old code called
// layerStore.Mount() under the read lock and panicked here. Concurrent read-locked Diffs must
// each produce a correct archive and balance their own driver references.
func TestLayerStoreDiffOnReadOnlyStore(t *testing.T) {
	rw, _, graphHome, _, layerDir := newLayerStoreForDiffTest(t)
	const id = "ro-diff-layer"
	createDiffTestLayer(t, rw, id)
	options := uncompressedDiffOptions()

	// Build a read-only copy of the layer metadata directory, like an additional image store;
	// the graph driver resolves layer contents from the shared home.
	roRoot := filepath.Join(t.TempDir(), "additional-store")
	roLayerDir := filepath.Join(roRoot, "layers")
	require.NoError(t, os.MkdirAll(roLayerDir, 0o755))

	for _, name := range []string{"layers.json", id + tarSplitSuffix} {
		data, err := os.ReadFile(filepath.Join(layerDir, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(roLayerDir, name), data, 0o644))
	}
	roCounter, roDriver := newCountingDriver(t, graphHome)
	roStore, err := newROLayerStore(filepath.Join(roRoot, "run"), roLayerDir, roDriver)
	require.NoError(t, err)

	const goroutines = 4
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := func() error {
				if err := roStore.startReading(); err != nil {
					return err
				}
				defer roStore.stopReading()
				rc, err := roStore.Diff("", id, options)
				if err != nil {
					return err
				}
				data, readErr := io.ReadAll(rc)
				closeErr := rc.Close()
				if readErr != nil {
					return readErr
				}
				if closeErr != nil {
					return closeErr
				}
				entries, err := tarFileContents(data)
				if err != nil {
					return err
				}
				if entries["hello.txt"] != "hello\n" {
					return errors.New("read-only store Diff produced a wrong archive")
				}
				return nil
			}()
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	gets, puts := roCounter.counts(id)
	require.Equal(t, gets, puts, "read-only Diffs must balance their private driver references")
	require.Equal(t, goroutines, gets, "each read-only Diff must take exactly one driver.Get() reference")
}
