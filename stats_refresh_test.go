package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUnmappedTestStore returns a vfs store without UID/GID mappings, matching
// the plain ownership of the files created directly by the tests.
func newUnmappedTestStore(t *testing.T) Store {
	t.Helper()
	wd := t.TempDir()
	st, err := GetStore(StoreOptions{
		RunRoot:            filepath.Join(wd, "run"),
		GraphRoot:          filepath.Join(wd, "root"),
		GraphDriverName:    "vfs",
		GraphDriverOptions: []string{},
	})
	require.NoError(t, err)
	return st
}

func vfsStoreDirs(t *testing.T, st Store) (graphRoot, layerDir, imageDir string) {
	t.Helper()
	graphRoot = st.GraphRoot()
	return graphRoot, filepath.Join(graphRoot, "vfs-layers"), filepath.Join(graphRoot, "vfs-images")
}

func statsStoreOptions(t *testing.T, graphRoot, runRoot string) StoreOptions {
	return StoreOptions{
		RunRoot:            runRoot,
		GraphRoot:          graphRoot,
		GraphDriverName:    "vfs",
		GraphDriverOptions: []string{},
		UIDMap: []idtools.IDMap{{
			ContainerID: 0,
			HostID:      os.Getuid(),
			Size:        1,
		}},
		GIDMap: []idtools.IDMap{{
			ContainerID: 0,
			HostID:      os.Getgid(),
			Size:        1,
		}},
	}
}

// writeLayerFile places a file directly into a vfs layer directory, which is
// enough to make the driver report non-zero usage and diff size.  (The tests
// deliberately avoid ApplyDiff and its tar handling here.)
func writeLayerFile(t *testing.T, graphRoot, id, name, content string) {
	t.Helper()
	dir := filepath.Join(graphRoot, "vfs", "dir", id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// TestRefreshLayerStatistics covers the happy path: collection, validation,
// same-generation publication, idempotent repetition and statistics after
// the layer contents change.
func TestRefreshLayerStatistics(t *testing.T) {
	st := newUnmappedTestStore(t)
	graphRoot, layerDir, imageDir := vfsStoreDirs(t, st)

	// Before any refresh, there is no committed generation and status does
	// not advertise one.
	report, err := st.LayerStatistics()
	require.NoError(t, err)
	assert.Nil(t, report)
	initialStatus, err := st.Status()
	require.NoError(t, err)
	for _, pair := range initialStatus {
		assert.NotEqual(t, "Layer Statistics Generation", pair[0])
	}

	layer, err := st.CreateLayer("test-layer", "", nil, "", false, nil)
	require.NoError(t, err)
	image, err := st.CreateImage("test-image", nil, layer.ID, "", nil)
	require.NoError(t, err)

	// First refresh publishes a generation for the empty layer.
	first, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.True(t, first.Changed)
	assert.NotEmpty(t, first.Generation)
	require.Len(t, first.Layers, 1)
	assert.Equal(t, layer.ID, first.Layers[0].ID)
	assert.Equal(t, int64(0), first.Layers[0].Size)
	assert.Equal(t, int64(0), first.Layers[0].DiffSize)
	// The vfs driver does not use quota.
	assert.False(t, first.Layers[0].QuotaEnabled)
	// Filesystem capacity is collected on supported platforms.
	assert.NotZero(t, first.Filesystem.TotalBytes)

	// The marker is present, no candidate traces remain, and both indexes
	// carry the same generation.
	_, err = os.Stat(filepath.Join(layerDir, statsCommittedFileName))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(layerDir, statsSnapshotDirName))
	assert.True(t, os.IsNotExist(err))

	layersData, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
	require.NoError(t, err)
	assert.Contains(t, string(layersData), first.Generation)
	imagesData, err := os.ReadFile(filepath.Join(imageDir, "images.json"))
	require.NoError(t, err)
	assert.Contains(t, string(imagesData), first.Generation)

	// The in-memory layer belongs to the published generation; queries never
	// observe candidate state.
	gotLayer, err := st.Layer(layer.ID)
	require.NoError(t, err)
	assert.Equal(t, first.Generation, gotLayer.StatsGeneration)
	assert.Equal(t, int64(0), gotLayer.StatsSize)
	gotImage, err := st.Image(image.ID)
	require.NoError(t, err)
	assert.Equal(t, first.Generation, gotImage.StatsGeneration)

	committed, err := st.LayerStatistics()
	require.NoError(t, err)
	require.NotNil(t, committed)
	assert.Equal(t, first.Generation, committed.Generation)

	// Repeated refresh with identical statistics is a no-op.
	again, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	assert.False(t, again.Changed)
	assert.Equal(t, first.Generation, again.Generation)

	// Populate the layer: the next refresh publishes a newer generation and
	// never rolls back to the old (empty) one.
	writeLayerFile(t, graphRoot, layer.ID, "content.txt", "statistics generation test contents\n")
	second, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	require.True(t, second.Changed)
	assert.NotEqual(t, first.Generation, second.Generation)
	require.Len(t, second.Layers, 1)
	assert.Greater(t, second.Layers[0].Size, int64(0))
	assert.Greater(t, second.Layers[0].DiffSize, int64(0))
	assert.Equal(t, second.Layers[0].DiffSize, second.Layers[0].Size)

	gotLayer, err = st.Layer(layer.ID)
	require.NoError(t, err)
	assert.Equal(t, second.Generation, gotLayer.StatsGeneration)
	assert.Greater(t, gotLayer.StatsSize, int64(0))

	// Normal query paths keep working after generations are published, and
	// status surfaces only the committed generation.
	statusPairs, err := st.Status()
	require.NoError(t, err)
	statusByKey := map[string]string{}
	for _, pair := range statusPairs {
		statusByKey[pair[0]] = pair[1]
	}
	assert.Equal(t, second.Generation, statusByKey["Layer Statistics Generation"])
}

// TestRefreshStatisticsVerificationFailure ensures that failed reference
// verification publishes nothing and leaves the previous complete generation
// readable.
func TestRefreshStatisticsVerificationFailure(t *testing.T) {
	st := newUnmappedTestStore(t)
	graphRoot, layerDir, _ := vfsStoreDirs(t, st)

	layer, err := st.CreateLayer("inconsistent-layer", "", nil, "", false, nil)
	require.NoError(t, err)
	_, err = st.CreateImage("inconsistent-image", nil, layer.ID, "", nil)
	require.NoError(t, err)
	writeLayerFile(t, graphRoot, layer.ID, "content.txt", "statistics generation test contents\n")

	first, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	require.True(t, first.Changed)

	// Corrupt the image index through the live store so that it references a
	// layer which does not exist in any layer store.  This is exactly the
	// cross-index inconsistency that must prevent publication.
	corruptImageReferences := func(topLayer string) error {
		concrete := st.(*store)
		img := concrete.imageStore.(*imageStore)
		if err := img.startWriting(); err != nil {
			return err
		}
		defer img.stopWriting()
		for _, image := range img.images {
			image.TopLayer = topLayer
		}
		return img.Save()
	}
	require.NoError(t, corruptImageReferences("does-not-exist-layer"))

	_, err = st.RefreshLayerStatistics()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLayerUnknown), "expected layer reference verification error, got: %v", err)

	// No candidate survived, and the committed generation is still the old
	// complete one.
	_, err = os.Stat(filepath.Join(layerDir, statsSnapshotDirName))
	assert.True(t, os.IsNotExist(err))
	committed, err := st.LayerStatistics()
	require.NoError(t, err)
	require.NotNil(t, committed)
	assert.Equal(t, first.Generation, committed.Generation)

	// Restore the consistent index: normal operations keep working.
	require.NoError(t, corruptImageReferences(layer.ID))
	_, err = st.DiffSize("", layer.ID)
	require.NoError(t, err)
	_, err = st.Layers()
	require.NoError(t, err)
	_, err = st.Images()
	require.NoError(t, err)
	_, err = st.Status()
	require.NoError(t, err)
	report, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	assert.Equal(t, first.Generation, report.Generation)
}

// writeCandidateOnDisk materializes a candidate generation for recovery
// tests, exactly like statisticsManager.prepare would.  buildPayloads returns
// the candidate layer and image index payloads.
func writeCandidateOnDisk(t *testing.T, layerDir, imageDir, generation string, buildPayloads func() (layersPayload, imagesPayload []byte)) *statsSnapshotManifest {
	t.Helper()

	layersPath := filepath.Join(layerDir, "layers.json")
	imagesPath := filepath.Join(imageDir, "images.json")
	layersTarget := &statsSnapshotTarget{Kind: statsTargetLayers, Path: layersPath, LockPath: filepath.Join(layerDir, "layers.lock")}
	imagesTarget := &statsSnapshotTarget{Kind: statsTargetImages, Path: imagesPath, LockPath: filepath.Join(imageDir, "images.lock")}

	baseTarget := func(target *statsSnapshotTarget) {
		data, err := os.ReadFile(target.Path)
		if err != nil {
			require.True(t, os.IsNotExist(err))
			return
		}
		target.BasePresent = true
		target.BaseSize = int64(len(data))
		target.BaseSHA256 = checksum(data)
	}
	baseTarget(layersTarget)
	baseTarget(imagesTarget)

	layersPayload, imagesPayload := buildPayloads()

	manifest := &statsSnapshotManifest{
		Version:    statsSnapshotVersion,
		State:      statsSnapshotStateReady,
		Generation: generation,
		Driver:     "vfs",
		Layers:     *layersTarget,
		Images:     *imagesTarget,
	}
	manifest.Layers.TempName = statsCandidateTempPrefix + generation + "-layers.json"
	manifest.Images.TempName = statsCandidateTempPrefix + generation + "-images.json"

	layersTemp := filepath.Join(layerDir, manifest.Layers.TempName)
	imagesTemp := filepath.Join(imageDir, manifest.Images.TempName)
	require.NoError(t, writeSyncedFile(layersTemp, layersPayload, 0o600))
	require.NoError(t, writeSyncedFile(imagesTemp, imagesPayload, 0o600))
	manifest.Layers.Size = int64(len(layersPayload))
	manifest.Layers.SHA256 = checksum(layersPayload)
	manifest.Images.Size = int64(len(imagesPayload))
	manifest.Images.SHA256 = checksum(imagesPayload)

	dir := filepath.Join(layerDir, statsSnapshotDirName, generation)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	manifestData, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, writeSyncedFile(filepath.Join(dir, ".manifest.tmp"), manifestData, 0o600))
	require.NoError(t, os.Rename(filepath.Join(dir, ".manifest.tmp"), filepath.Join(dir, statsSnapshotManifestName)))
	require.NoError(t, syncDirectory(dir))
	return manifest
}

// rebaseCandidatePayloads returns a payload builder that stamps the generation
// onto copies of the current layer and image indexes.
func rebaseCandidatePayloads(t *testing.T, layerDir, imageDir, generation string) func() ([]byte, []byte) {
	return func() ([]byte, []byte) {
		var layers []*Layer
		data, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &layers))
		for _, l := range layers {
			l.StatsGeneration = generation
			l.StatsSize = 7
		}
		layersPayload, err := json.Marshal(&layers)
		require.NoError(t, err)

		var images []*Image
		data, err = os.ReadFile(filepath.Join(imageDir, "images.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &images))
		for _, i := range images {
			i.StatsGeneration = generation
		}
		imagesPayload, err := json.Marshal(&images)
		require.NoError(t, err)
		return layersPayload, imagesPayload
	}
}

func runRecoveryWithLocks(t *testing.T, layerDir, imageDir string) {
	t.Helper()
	ll, err := lockfile.GetLockFile(filepath.Join(layerDir, "layers.lock"))
	require.NoError(t, err)
	ll.Lock()
	defer ll.Unlock()
	il, err := lockfile.GetLockFile(filepath.Join(imageDir, "images.lock"))
	require.NoError(t, err)
	il.Lock()
	defer il.Unlock()
	require.NoError(t, recoverStatisticsSnapshotsLocked(layerDir, imageDir, ll, il))
}

func assertNoCandidateLeftovers(t *testing.T, layerDir, imageDir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(layerDir, statsSnapshotDirName))
	assert.True(t, os.IsNotExist(err), "snapshot directory still exists")
	entries, err := os.ReadDir(layerDir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), statsCandidateTempPrefix)
	}
}

// TestStatisticsSnapshotRecovery exercises startup recovery in all of its
// deterministic branches.
func TestStatisticsSnapshotRecovery(t *testing.T) {
	st := newTestStore(t, StoreOptions{})
	graphRoot, layerDir, imageDir := vfsStoreDirs(t, st)

	layer, err := st.CreateLayer("recover-layer", "", nil, "", false, nil)
	require.NoError(t, err)
	_, err = st.CreateImage("recover-image", nil, layer.ID, "", nil)
	require.NoError(t, err)
	committed, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	require.True(t, committed.Changed)

	t.Run("prepared candidate is completed", func(t *testing.T) {
		gen := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		writeCandidateOnDisk(t, layerDir, imageDir, gen,
			rebaseCandidatePayloads(t, layerDir, imageDir, gen))

		// Queries only see the old committed generation while the candidate
		// is merely prepared.
		before, err := st.LayerStatistics()
		require.NoError(t, err)
		require.NotNil(t, before)
		assert.Equal(t, committed.Generation, before.Generation)

		runRecoveryWithLocks(t, layerDir, imageDir)

		after, err := st.LayerStatistics()
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.Equal(t, gen, after.Generation)
		layersData, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
		require.NoError(t, err)
		assert.Contains(t, string(layersData), gen)
		assertNoCandidateLeftovers(t, layerDir, imageDir)
	})

	t.Run("partially published candidate is completed", func(t *testing.T) {
		gen := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		manifest := writeCandidateOnDisk(t, layerDir, imageDir, gen,
			rebaseCandidatePayloads(t, layerDir, imageDir, gen))
		// Simulate a crash after the layer lock was advanced and the layer
		// index renamed, but before the image index rename.
		ll, err := lockfile.GetLockFile(filepath.Join(layerDir, "layers.lock"))
		require.NoError(t, err)
		ll.Lock()
		_, err = ll.RecordWrite()
		require.NoError(t, err)
		require.NoError(t, os.Rename(
			filepath.Join(layerDir, manifest.Layers.TempName),
			filepath.Join(layerDir, "layers.json")))
		ll.Unlock()

		runRecoveryWithLocks(t, layerDir, imageDir)

		after, err := st.LayerStatistics()
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.Equal(t, gen, after.Generation)
		imagesData, err := os.ReadFile(filepath.Join(imageDir, "images.json"))
		require.NoError(t, err)
		assert.Contains(t, string(imagesData), gen)
		assertNoCandidateLeftovers(t, layerDir, imageDir)
	})

	t.Run("corrupt candidate is discarded", func(t *testing.T) {
		gen := "cccccccccccccccccccccccccccccccc"
		manifest := writeCandidateOnDisk(t, layerDir, imageDir, gen,
			rebaseCandidatePayloads(t, layerDir, imageDir, gen))
		// Corrupt the layer payload after the manifest was written.
		require.NoError(t, os.WriteFile(
			filepath.Join(layerDir, manifest.Layers.TempName), []byte("not json"), 0o600))

		runRecoveryWithLocks(t, layerDir, imageDir)

		after, err := st.LayerStatistics()
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.NotEqual(t, gen, after.Generation)
		layersData, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
		require.NoError(t, err)
		assert.NotContains(t, string(layersData), gen)
		assertNoCandidateLeftovers(t, layerDir, imageDir)
	})

	t.Run("superseded candidate is discarded without touching indexes", func(t *testing.T) {
		gen := "dddddddddddddddddddddddddddddddd"
		otherGen := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		writeCandidateOnDisk(t, layerDir, imageDir, gen,
			rebaseCandidatePayloads(t, layerDir, imageDir, gen))

		// An unrelated newer write takes over the layer index.
		var layers []*Layer
		data, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &layers))
		require.NotEmpty(t, layers)
		layers[0].StatsGeneration = otherGen
		layers[0].StatsSize = 999
		otherPayload, err := json.Marshal(&layers)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(layerDir, "layers.json"), otherPayload, 0o600))

		runRecoveryWithLocks(t, layerDir, imageDir)

		// The newer write survives, the candidate is gone, and no marker for
		// either the candidate or a rollback to the older state exists.
		kept, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
		require.NoError(t, err)
		assert.Contains(t, string(kept), otherGen)
		assert.NotContains(t, string(kept), gen)
		after, err := st.LayerStatistics()
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.NotEqual(t, gen, after.Generation)
		assertNoCandidateLeftovers(t, layerDir, imageDir)

		// Restore a consistent index for following tests by refreshing.
		require.NoError(t, os.WriteFile(filepath.Join(layerDir, "layers.json"), data, 0o600))
		rep, err := st.RefreshLayerStatistics()
		require.NoError(t, err)
		_ = rep
	})

	t.Run("orphan temp files are swept", func(t *testing.T) {
		orphan := filepath.Join(layerDir, statsCandidateTempPrefix+"orphan123-layers.json")
		require.NoError(t, os.WriteFile(orphan, []byte("[]"), 0o600))
		runRecoveryWithLocks(t, layerDir, imageDir)
		_, err := os.Stat(orphan)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("recovery runs automatically when the store is reopened", func(t *testing.T) {
		opts := statsStoreOptions(t, graphRoot, st.RunRoot())
		gen := "ffffffffffffffffffffffffffffffff"
		writeCandidateOnDisk(t, layerDir, imageDir, gen,
			rebaseCandidatePayloads(t, layerDir, imageDir, gen))

		st.Free()
		reopened, err := GetStore(opts)
		require.NoError(t, err)

		report, err := reopened.LayerStatistics()
		require.NoError(t, err)
		require.NotNil(t, report)
		assert.Equal(t, gen, report.Generation)

		got, err := reopened.Layer(layer.ID)
		require.NoError(t, err)
		assert.Equal(t, gen, got.StatsGeneration)
		assertNoCandidateLeftovers(t, layerDir, imageDir)
	})
}

// TestStatisticsLegacyMetadata verifies that metadata written without
// statistics fields loads unchanged.
func TestStatisticsLegacyMetadata(t *testing.T) {
	st := newTestStore(t, StoreOptions{})
	_, layerDir, imageDir := vfsStoreDirs(t, st)

	// Pre-populate through the API, then strip the statistics fields like an
	// old library version would have written them.
	layer, err := st.CreateLayer("legacy-layer", "", nil, "", false, nil)
	require.NoError(t, err)
	_, err = st.CreateImage("legacy-image", nil, layer.ID, "", nil)
	require.NoError(t, err)
	_, err = st.RefreshLayerStatistics()
	require.NoError(t, err)

	var layers []*Layer
	data, err := os.ReadFile(filepath.Join(layerDir, "layers.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &layers))
	var images []*Image
	idata, err := os.ReadFile(filepath.Join(imageDir, "images.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(idata, &images))

	for _, l := range layers {
		l.StatsGeneration = ""
		l.StatsSize = 0
		l.StatsInodes = 0
		l.StatsDiffSize = 0
		l.StatsQuotaEnabled = false
		l.StatsAt = time.Time{}
	}
	legacyLayers, err := json.Marshal(&layers)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(layerDir, "layers.json"), legacyLayers, 0o600))

	// A refresh on top of legacy metadata works and upgrades the indexes to a
	// generation.
	report, err := st.RefreshLayerStatistics()
	require.NoError(t, err)
	require.True(t, report.Changed)
	got, err := st.Layer(layer.ID)
	require.NoError(t, err)
	assert.Equal(t, report.Generation, got.StatsGeneration)
}
