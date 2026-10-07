package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	drivers "github.com/containers/storage/drivers"
	"github.com/containers/storage/internal/tempdir"
	"github.com/containers/storage/pkg/reexec"
	"github.com/containers/storage/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostIDMappingOptions avoids chroot-based ID-mapped layer copies, which
// require privileges unavailable in unprivileged test environments.
func hostIDMappingOptions() types.IDMappingOptions {
	return types.IDMappingOptions{HostUIDMapping: true, HostGIDMapping: true}
}

// concreteStore returns the concrete *store; the name avoids shadowing the
// store type with the common test variable name.
func concreteStore(s Store) *store {
	return s.(*store)
}

// newCleanupTestStore creates a vfs store without injecting UID/GID maps.
// With identity (host) mappings the vfs driver never has to chown layer
// contents through a privileged chroot subprocess, so the tests run in
// unprivileged environments.
func newCleanupTestStore(t *testing.T) Store {
	t.Helper()
	wd := t.TempDir()
	store, err := GetStore(StoreOptions{
		RunRoot:            filepath.Join(wd, "run"),
		GraphRoot:          filepath.Join(wd, "root"),
		GraphDriverName:    "vfs",
		GraphDriverOptions: []string{},
	})
	require.NoError(t, err)
	return store
}

var errJournalMissingForTest = errors.New("cleanup journal missing")

func testTime() time.Time {
	return time.Now().UTC()
}

func readJournalForTest(store Store) (*cleanupJournal, error) {
	data, err := os.ReadFile(filepath.Join(cleanupJournalDir(store), cleanupFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errJournalMissingForTest
		}
		return nil, err
	}
	journal := &cleanupJournal{}
	if err := json.Unmarshal(data, journal); err != nil {
		return nil, err
	}
	return journal, nil
}

// cleanupFixture builds a three-layer chain, an image on top of it and a
// container using the image, returning all of their IDs.
func cleanupFixture(t *testing.T, store Store, prefix string) (base, middle, top, imageID, containerID string) {
	t.Helper()
	reexec.Init()
	base = prefix + "-base"
	middle = prefix + "-middle"
	top = prefix + "-top"
	imageID = prefix + "-image"
	containerID = prefix + "-container"

	_, err := store.CreateLayer(base, "", nil, "", false, nil)
	require.NoError(t, err)
	_, err = store.CreateLayer(middle, base, nil, "", false, nil)
	require.NoError(t, err)
	_, err = store.CreateLayer(top, middle, nil, "", false, nil)
	require.NoError(t, err)
	_, err = store.CreateImage(imageID, nil, top, "", nil)
	require.NoError(t, err)
	c, err := store.CreateContainer(containerID, nil, imageID, "", "", &ContainerOptions{
		IDMappingOptions: hostIDMappingOptions(),
	})
	require.NoError(t, err)
	require.Equal(t, containerID, c.ID)
	return base, middle, top, imageID, containerID
}

func cleanupJournalDir(store Store) string {
	return filepath.Join(store.GraphRoot(), store.GraphDriverName()+"-"+cleanupDirSuffix)
}

// TestCleanupContainersFull removes a container plus its image and the full
// layer chain and verifies that nothing remains, including on disk.
func TestCleanupContainersFull(t *testing.T) {
	store := newCleanupTestStore(t)
	base, middle, top, imageID, containerID := cleanupFixture(t, store, "full")

	container, err := store.Container(containerID)
	require.NoError(t, err)
	containerLayer := container.LayerID

	report, err := store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs:             []string{containerID},
		RemoveUnreferencedImages: true,
	})
	require.NoError(t, err)
	assert.False(t, report.Resumed)
	assert.ElementsMatch(t, []string{containerID}, report.ContainersRemoved)
	assert.ElementsMatch(t, []string{imageID}, report.ImagesRemoved)
	assert.ElementsMatch(t, []string{base, middle, top, containerLayer}, report.LayersRemoved)
	assert.Empty(t, report.LayersPreserved)

	for _, id := range []string{containerID, imageID, base, middle, top, containerLayer} {
		assert.False(t, store.Exists(id), "%s should be gone", id)
	}
	layers, err := store.Layers()
	require.NoError(t, err)
	assert.Empty(t, layers)

	// A second run over the same (already gone) IDs is idempotent.
	second, err := store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs:             []string{containerID},
		RemoveUnreferencedImages: true,
	})
	require.NoError(t, err)
	assert.Empty(t, second.ContainersRemoved)
	assert.Empty(t, second.ImagesRemoved)
	assert.Empty(t, second.LayersRemoved)

	// The completed journal is removed.
	_, err = readJournalForTest(store)
	assert.ErrorIs(t, err, errJournalMissingForTest)

	// The generation fence file is retained (it is the tombstone history).
	concrete := concreteStore(store)
	rls, err := concrete.getLayerStore()
	require.NoError(t, err)
	require.NoError(t, rls.startWriting())
	assert.Equal(t, uint64(1), rls.layerGeneration(base))
	rls.stopWriting()
}

// TestCleanupContainersSharedImage verifies that an image (and its chain)
// used by a surviving container is preserved, while only the removed
// container's writable layer is collected.
func TestCleanupContainersSharedImage(t *testing.T) {
	store := newCleanupTestStore(t)
	base, middle, top, imageID, containerID := cleanupFixture(t, store, "shared")

	// A second container on the same image.
	other, err := store.CreateContainer("shared-other", nil, imageID, "", "", &ContainerOptions{
		IDMappingOptions: hostIDMappingOptions(),
	})
	require.NoError(t, err)

	report, err := store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs:             []string{containerID},
		RemoveUnreferencedImages: true,
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{containerID}, report.ContainersRemoved)
	// The image was never a removal candidate because the surviving
	// container still references it.
	assert.Empty(t, report.ImagesRemoved)
	assert.Empty(t, report.ImagesPreserved)
	// The shared chain was never scheduled for removal (the image stayed),
	// so it is simply untouched rather than reported as preserved.
	assert.Empty(t, report.LayersPreserved)

	assert.True(t, store.Exists(imageID))
	assert.True(t, store.Exists(other.ID))
	for _, id := range []string{base, middle, top, other.LayerID} {
		_, err := store.Layer(id)
		require.NoError(t, err, "layer %s must survive", id)
	}
}

// TestCleanupContainersMounted verifies that a locked (mounted) container
// is preserved, and that the same cleanup succeeds once it is unmounted.
func TestCleanupContainersMounted(t *testing.T) {
	store := newCleanupTestStore(t)
	_, _, _, imageID, containerID := cleanupFixture(t, store, "mounted")

	mountPoint, err := store.Mount(containerID, "")
	require.NoError(t, err)
	require.NotEmpty(t, mountPoint)

	report, err := store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs:             []string{containerID},
		RemoveUnreferencedImages: true,
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{containerID}, report.ContainersPreserved)
	assert.True(t, store.Exists(containerID))
	assert.True(t, store.Exists(imageID))

	_, err = store.Unmount(containerID, true)
	require.NoError(t, err)

	report, err = store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs:             []string{containerID},
		RemoveUnreferencedImages: true,
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{containerID}, report.ContainersRemoved)
	assert.False(t, store.Exists(containerID))
	assert.False(t, store.Exists(imageID))
}

// TestCleanupDryRun verifies that a dry run reports the intent without
// removing anything or persisting a journal.
func TestCleanupDryRun(t *testing.T) {
	store := newCleanupTestStore(t)
	base, middle, top, imageID, containerID := cleanupFixture(t, store, "dry")
	container, err := store.Container(containerID)
	require.NoError(t, err)

	report, err := store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs:             []string{containerID},
		RemoveUnreferencedImages: true,
		DryRun:                   true,
	})
	require.NoError(t, err)
	assert.True(t, report.DryRun)
	assert.ElementsMatch(t, []string{containerID}, report.ContainersRemoved)
	assert.ElementsMatch(t, []string{imageID}, report.ImagesRemoved)
	assert.ElementsMatch(t, []string{base, middle, top, container.LayerID}, report.LayersRemoved)

	assert.True(t, store.Exists(containerID))
	assert.True(t, store.Exists(imageID))
	_, err = readJournalForTest(store)
	assert.ErrorIs(t, err, errJournalMissingForTest)
}

// TestCleanupUnknownContainerIsNoOp verifies idempotent handling of
// container IDs that no longer exist.
func TestCleanupUnknownContainerIsNoOp(t *testing.T) {
	store := newCleanupTestStore(t)
	_, _, _, _, containerID := cleanupFixture(t, store, "unknown")

	report, err := store.CleanupContainers(CleanupContainersOptions{
		ContainerIDs: []string{"does-not-exist"},
	})
	require.NoError(t, err)
	assert.Empty(t, report.ContainersRemoved)
	assert.Empty(t, report.ContainersPreserved)
	assert.True(t, store.Exists(containerID))
	_, err = readJournalForTest(store)
	assert.ErrorIs(t, err, errJournalMissingForTest)
}

// TestCleanupJournalResume simulates a process that removed a container
// (and committed that) but was killed before removing the image and orphan
// layers. The next cleanup resumes from the persisted journal.
func TestCleanupJournalResume(t *testing.T) {
	store := newCleanupTestStore(t)
	concrete := concreteStore(store)
	base, middle, top, imageID, containerID := cleanupFixture(t, store, "resume")
	container, err := store.Container(containerID)
	require.NoError(t, err)
	containerLayer := container.LayerID

	// Interrupted run state: container and its writable layer are gone,
	// but the image and its chain are still on disk.
	require.NoError(t, store.DeleteContainer(containerID))
	_, err = store.Layer(containerLayer)
	require.Error(t, err)
	_, err = store.Image(imageID)
	require.NoError(t, err)

	// Write the journal the killed process would have left behind, at the
	// image phase.
	rls, err := concrete.getLayerStore()
	require.NoError(t, err)
	require.NoError(t, rls.startWriting())
	genBase := rls.layerGeneration(base)
	genMiddle := rls.layerGeneration(middle)
	genTop := rls.layerGeneration(top)
	rls.stopWriting()
	require.NotZero(t, genBase)

	manager, err := concrete.startCleanup()
	require.NoError(t, err)
	journal := &cleanupJournal{
		Version: cleanupJournalVersion,
		ID:      "resume-journal",
		Created: testTime(),
		Phase:   phaseImages,
		Snapshot: cleanupSnapshot{
			Containers: map[string]cleanupContainerRef{
				containerID: {ImageID: imageID, LayerID: containerLayer},
			},
			Images: map[string]cleanupImageRef{
				imageID: {TopLayer: top},
			},
			LayerParents: map[string]string{base: "", middle: base, top: middle, containerLayer: top},
		},
		Containers: []cleanupContainerRecord{{
			ID:      containerID,
			ImageID: imageID,
			LayerID: containerLayer,
			State:   stateDone,
		}},
		Images: []cleanupImageRecord{{ID: imageID, State: statePending}},
		Layers: map[string]*cleanupLayerRecord{
			base:   {ID: base, Generation: genBase, State: statePending},
			middle: {ID: middle, Parent: base, Generation: genMiddle, State: statePending},
			top:    {ID: top, Parent: middle, Generation: genTop, State: statePending},
		},
	}
	require.NoError(t, manager.saveJournal(journal))
	manager.unlock()

	// The next cleanup (here explicitly, as GetStore would do on reopen)
	// resumes without a new intent.
	require.NoError(t, concrete.resumePendingCleanup())

	assert.False(t, store.Exists(imageID))
	for _, id := range []string{base, middle, top} {
		assert.False(t, store.Exists(id), "layer %s must be removed on resume", id)
	}
	_, err = readJournalForTest(store)
	assert.ErrorIs(t, err, errJournalMissingForTest)
}

// TestCleanupGenerationFence verifies that a layer which was scheduled for
// deletion but then re-created (in a newer generation) is preserved and its
// new driver directory is never removed by the resumed cleanup.
func TestCleanupGenerationFence(t *testing.T) {
	store := newCleanupTestStore(t)
	concrete := concreteStore(store)
	reexec.Init()

	layerID := "fence-layer"
	imageID := "fence-image"
	_, err := store.CreateLayer(layerID, "", nil, "", false, nil)
	require.NoError(t, err)
	_, err = store.CreateImage(imageID, nil, layerID, "", nil)
	require.NoError(t, err)

	rls, err := concrete.getLayerStore()
	require.NoError(t, err)
	require.NoError(t, rls.startWriting())
	genOne := rls.layerGeneration(layerID)
	require.Equal(t, uint64(1), genOne)
	rls.stopWriting()

	// Persist a journal that scheduled layerID at generation 1.
	manager, err := concrete.startCleanup()
	require.NoError(t, err)
	journal := &cleanupJournal{
		Version: cleanupJournalVersion,
		ID:      "fence-journal",
		Created: testTime(),
		Phase:   phaseImages,
		Snapshot: cleanupSnapshot{
			Images:       map[string]cleanupImageRef{imageID: {TopLayer: layerID}},
			LayerParents: map[string]string{layerID: ""},
		},
		Images: []cleanupImageRecord{{ID: imageID, State: statePending}},
		Layers: map[string]*cleanupLayerRecord{
			layerID: {ID: layerID, Generation: genOne, State: statePending},
		},
	}
	require.NoError(t, manager.saveJournal(journal))
	manager.unlock()

	// Simulate the interrupted phase-4 state: index entry and image gone,
	// driver directory staged for removal but not yet removed; then the
	// layer is pulled again (same content-addressed ID, new directory,
	// generation 2).
	var stagedCleanups []tempdir.CleanupTempDirFunc
	require.NoError(t, concrete.writeToAllStores(func(rlstore rwLayerStore) error {
		cleanups, err := rlstore.deferredDelete(layerID)
		stagedCleanups = append(stagedCleanups, cleanups...)
		if err != nil {
			return err
		}
		return concrete.imageStore.Delete(imageID)
	}))
	_, err = store.CreateLayer(layerID, "", nil, "", false, nil)
	require.NoError(t, err)

	require.NoError(t, rls.startWriting())
	assert.Equal(t, uint64(2), rls.layerGeneration(layerID))
	rls.stopWriting()

	report := CleanupReport{Resumed: true}
	require.NoError(t, concrete.resumePendingCleanupWithReport(&report))
	assert.ElementsMatch(t, []string{imageID}, report.ImagesRemoved)
	assert.ElementsMatch(t, []string{layerID}, report.LayersPreserved)

	// The re-created layer and its fresh driver directory survive.
	l, err := store.Layer(layerID)
	require.NoError(t, err)
	assert.Equal(t, layerID, l.ID)
	driver, err := store.GraphDriver()
	require.NoError(t, err)
	dir, err := driver.Get(layerID, drivers.MountOpts{})
	require.NoError(t, err)
	require.DirExists(t, dir)

	// Finishing the staged removal of the OLD directory must not affect
	// the new generation's directory.
	require.NoError(t, tempdir.CleanupTemporaryDirectories(stagedCleanups...))
	dir2, err := driver.Get(layerID, drivers.MountOpts{})
	require.NoError(t, err)
	assert.Equal(t, dir, dir2)
}
