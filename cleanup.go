package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/containers/storage/internal/tempdir"
	"github.com/containers/storage/pkg/ioutils"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/containers/storage/pkg/stringid"
	"github.com/containers/storage/pkg/system"
	"github.com/sirupsen/logrus"
)

// This file implements crash-recoverable batch cleanup.
//
// A cleanup run builds two durable pieces of state before touching anything:
//
//   - a reference snapshot, describing every container->image/layer and
//     image->layer reference (and the layer parent chain) that existed when
//     the cleanup was planned, and
//   - the cleanup intent, i.e. the ordered records describing which
//     containers, images and layers the run intends to remove.
//
// The intent is advanced strictly in dependency order
// (containers -> images -> mounts -> layer index -> driver directories) and
// every transition is persisted to an atomic, fsync'ed journal in the graph
// root.  Each destructive step is itself idempotent, so a process that is
// killed at any point can resume from the journal without deleting anything
// twice or recreating ("resurrecting") an already committed deletion.
//
// Layers carry a persistent creation generation (see layerStore).  A layer
// is physically removed only while its generation still matches the one the
// journal captured: a layer ID that was re-created (pulled again) after the
// deletion was scheduled belongs to a newer generation and is preserved.
// All mutations happen under the existing hierarchical store write locks
// and the existing atomic metadata writers, so concurrent image creation is
// serialized with the cleanup and readers can never observe a half-deleted
// image or layer.

const (
	cleanupJournalVersion = 1
	// cleanupDirSuffix is appended to the graph driver name to name the
	// directory (under GraphRoot) that holds the cleanup journal and lock.
	cleanupDirSuffix = "cleanup"
	cleanupFileName  = "cleanup.json"
	cleanupLockName  = "cleanup.lock"

	// cleanupSkipMounted is recorded when a container cannot be removed
	// because its layer is mounted.
	cleanupSkipMounted = "container layer is mounted"
)

// cleanupPhase is the journal cursor.  Phases advance strictly in the
// declared order; the phase together with the per-item states is what makes
// a resume after a crash unambiguous.
type cleanupPhase string

const (
	phaseContainers cleanupPhase = "containers"
	phaseImages     cleanupPhase = "images"
	phaseMounts     cleanupPhase = "mounts"
	phaseLayerIndex cleanupPhase = "layer-index"
	phaseDriverDirs cleanupPhase = "driver-directories"
	phaseComplete   cleanupPhase = "complete"
)

// cleanupItemState is the state of a single item recorded in the journal.
type cleanupItemState string

const (
	statePending   cleanupItemState = "pending"
	stateDone      cleanupItemState = "done"
	statePreserved cleanupItemState = "preserved"
)

// CleanupContainersOptions controls a crash-recoverable batch cleanup.
type CleanupContainersOptions struct {
	// ContainerIDs lists the containers (typically exited containers
	// selected by the node administrator) to remove. Ignored when
	// AllContainers is set.
	ContainerIDs []string

	// AllContainers removes every container known to the store.
	AllContainers bool

	// RemoveUnreferencedImages also removes the images that the removed
	// containers were using once no remaining container refers to them,
	// and schedules the image layer chains that become unreferenced for
	// removal. Images still in use are always preserved.
	RemoveUnreferencedImages bool

	// DryRun computes and reports the cleanup intent without persisting a
	// journal or removing anything.
	DryRun bool
}

// CleanupReport describes what a cleanup run did, or would do (DryRun).
type CleanupReport struct {
	// JournalID identifies the persisted journal that was executed.
	JournalID string `json:"-"`

	// Resumed is true if the run continued a journal left behind by a
	// process that failed or was killed mid-cleanup.
	Resumed bool `json:"resumed"`

	// DryRun is true if nothing was removed.
	DryRun bool `json:"dryRun"`

	ContainersRemoved   []string `json:"containersRemoved,omitempty"`
	ContainersPreserved []string `json:"containersPreserved,omitempty"`
	ImagesRemoved       []string `json:"imagesRemoved,omitempty"`
	ImagesPreserved     []string `json:"imagesPreserved,omitempty"`
	LayersRemoved       []string `json:"layersRemoved,omitempty"`
	LayersPreserved     []string `json:"layersPreserved,omitempty"`
}

type cleanupContainerRef struct {
	ImageID string `json:"image,omitempty"`
	LayerID string `json:"layer,omitempty"`
}

type cleanupImageRef struct {
	TopLayer        string   `json:"topLayer,omitempty"`
	MappedTopLayers []string `json:"mappedTopLayers,omitempty"`
}

// cleanupSnapshot is the durable reference snapshot captured before any
// removal starts.
type cleanupSnapshot struct {
	Containers   map[string]cleanupContainerRef `json:"containers"`
	Images       map[string]cleanupImageRef     `json:"images"`
	LayerParents map[string]string              `json:"layerParents,omitempty"`
}

type cleanupContainerRecord struct {
	ID      string           `json:"id"`
	ImageID string           `json:"image,omitempty"`
	LayerID string           `json:"layer,omitempty"`
	State   cleanupItemState `json:"state"`
	Reason  string           `json:"reason,omitempty"`
}

type cleanupImageRecord struct {
	ID     string           `json:"id"`
	State  cleanupItemState `json:"state"`
	Reason string           `json:"reason,omitempty"`
}

type cleanupLayerRecord struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	// Origin records which container or image made this layer a removal
	// candidate; it is diagnostic only.
	Origin string `json:"origin,omitempty"`
	// Generation is the layer creation generation captured when the
	// removal was scheduled.
	Generation uint64 `json:"generation"`

	Unmounted        bool `json:"unmounted,omitempty"`
	IndexRemoved     bool `json:"indexRemoved,omitempty"`
	DriverDirRemoved bool `json:"driverDirRemoved,omitempty"`

	State  cleanupItemState `json:"state"`
	Reason string           `json:"reason,omitempty"`
}

// cleanupJournal is the persisted reference snapshot, cleanup intent and
// progress cursor for one cleanup run.
type cleanupJournal struct {
	Version int                      `json:"version"`
	ID      string                   `json:"id"`
	Created time.Time                `json:"created"`
	Updated time.Time                `json:"updated"`
	Options CleanupContainersOptions `json:"options,omitempty"`
	Phase   cleanupPhase             `json:"phase"`

	Snapshot cleanupSnapshot `json:"snapshot"`

	Containers []cleanupContainerRecord `json:"containers"`
	Images     []cleanupImageRecord     `json:"images,omitempty"`
	// Layers is keyed by layer ID so that a layer reached from several
	// images/containers is scheduled exactly once.
	Layers map[string]*cleanupLayerRecord `json:"layers"`
}

// cleanupManager owns the cleanup journal lock and journal persistence for
// one invocation. Its cross-process lockfile serializes concurrent cleanup
// runs; the ordinary layer/image/container store locks serialize cleanup
// against every other mutation.
type cleanupManager struct {
	s   *store
	dir string
	// lock serializes whole cleanup runs (including resume) across
	// processes and goroutines.
	lock *lockfile.LockFile
}

func (s *store) cleanupDirectory() string {
	return filepath.Join(s.graphRoot, s.graphDriverName+"-"+cleanupDirSuffix)
}

// startCleanup creates the persistent cleanup area and acquires its lock.
// Failing to create the area (e.g. a read-only graph root) aborts cleanup
// without touching the store.
func (s *store) startCleanup() (*cleanupManager, error) {
	dir := s.cleanupDirectory()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating cleanup state directory %q: %w", dir, err)
	}
	lf, err := lockfile.GetLockFile(filepath.Join(dir, cleanupLockName))
	if err != nil {
		return nil, err
	}
	lf.Lock()
	return &cleanupManager{s: s, dir: dir, lock: lf}, nil
}

func (m *cleanupManager) unlock() {
	if m != nil && m.lock != nil {
		m.lock.Unlock()
	}
}

func (m *cleanupManager) journalPath() string {
	return filepath.Join(m.dir, cleanupFileName)
}

// loadJournal reads the persisted journal. A missing journal is reported as
// (nil, nil).
func (m *cleanupManager) loadJournal() (*cleanupJournal, error) {
	data, err := os.ReadFile(m.journalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	journal := &cleanupJournal{}
	if err := json.Unmarshal(data, journal); err != nil {
		return nil, fmt.Errorf("loading cleanup journal %q: %w", m.journalPath(), err)
	}
	if journal.Version != cleanupJournalVersion {
		return nil, fmt.Errorf("unsupported cleanup journal version %d in %q", journal.Version, m.journalPath())
	}
	if journal.Layers == nil {
		journal.Layers = map[string]*cleanupLayerRecord{}
	}
	return journal, nil
}

// saveJournal atomically persists the journal. It is called after every
// committed state transition, so a crash always leaves a journal whose
// records are no further along than the on-disk store state: resuming it
// retries the step that was in flight.
func (m *cleanupManager) saveJournal(journal *cleanupJournal) error {
	journal.Version = cleanupJournalVersion
	journal.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(journal, "", " ")
	if err != nil {
		return err
	}
	return ioutils.AtomicWriteFile(m.journalPath(), data, 0o600)
}

func (m *cleanupManager) removeJournal() error {
	if err := os.Remove(m.journalPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CleanupContainers removes a batch of (typically exited) containers and,
// optionally, the images and layers that become unreferenced as a result.
//
// The whole operation is driven by a persistent journal. If a previous run
// was interrupted, its journal is resumed first; every step is idempotent
// so resuming neither deletes anything twice nor resurrects references that
// were already committed as removed. Content referenced by surviving
// containers, images, child layers or a newer layer generation is always
// preserved.
func (s *store) CleanupContainers(options CleanupContainersOptions) (CleanupReport, error) {
	report := CleanupReport{DryRun: options.DryRun}

	manager, err := s.startCleanup()
	if err != nil {
		return report, err
	}
	defer manager.unlock()

	// Continue a journal left behind by a killed/failed run before doing
	// anything new.
	journal, err := manager.loadJournal()
	if err != nil {
		return report, err
	}
	if journal != nil && journal.Phase != phaseComplete {
		report.Resumed = true
		if err := manager.runJournal(journal, &report); err != nil {
			return report, err
		}
	} else if journal != nil {
		// A fully completed journal whose file was not removed (crash at
		// the very end) just needs to be discarded.
		if err := manager.removeJournal(); err != nil {
			return report, err
		}
	}

	if !options.AllContainers && len(options.ContainerIDs) == 0 {
		return report, nil
	}

	journal, err = manager.plan(options)
	if err != nil {
		return report, err
	}
	report.JournalID = journal.ID

	if options.DryRun {
		manager.fillReport(journal, &report)
		return report, nil
	}

	if err := manager.saveJournal(journal); err != nil {
		return report, err
	}
	if err := manager.runJournal(journal, &report); err != nil {
		return report, err
	}
	return report, nil
}

// resumePendingCleanup continues any journal left behind by an interrupted
// cleanup run. It is best effort: a failure here is logged by the caller
// and must not prevent the store from opening, matching the store's
// existing crash-recovery behavior.
func (s *store) resumePendingCleanup() error {
	return s.resumePendingCleanupWithReport(&CleanupReport{Resumed: true})
}

func (s *store) resumePendingCleanupWithReport(report *CleanupReport) error {
	dir := s.cleanupDirectory()
	// Never create anything on the auto-resume path: on a read-only graph
	// root (common with additional read-only stores) there is nothing to
	// resume anyway.
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lf, err := lockfile.GetLockFile(filepath.Join(dir, cleanupLockName))
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			logrus.Debugf("Not resuming storage cleanup in %q: %v", dir, err)
			return nil
		}
		return err
	}
	lf.Lock()
	defer lf.Unlock()
	manager := &cleanupManager{s: s, dir: dir, lock: lf}
	journal, err := manager.loadJournal()
	if err != nil {
		return err
	}
	if journal == nil {
		return nil
	}
	if journal.Phase == phaseComplete {
		return manager.removeJournal()
	}
	logrus.Warnf("Resuming interrupted storage cleanup %q", journal.ID)
	if report == nil {
		report = &CleanupReport{}
	}
	report.Resumed = true
	return manager.runJournal(journal, report)
}

// runJournal executes the journal phases in dependency order. It is safe to
// call repeatedly: already-committed records are skipped.
func (m *cleanupManager) runJournal(journal *cleanupJournal, report *CleanupReport) error {
	report.JournalID = journal.ID

	phaseRunners := []struct {
		phase cleanupPhase
		run   func(journal *cleanupJournal) ([]tempdir.CleanupTempDirFunc, error)
	}{
		{phaseContainers, m.phaseContainers},
		{phaseImages, m.phaseImages},
		{phaseMounts, m.phaseMounts},
		{phaseLayerIndex, m.phaseLayerIndex},
	}

	var deferredPhysicalRemovals []tempdir.CleanupTempDirFunc
	for _, step := range phaseRunners {
		if phaseOrder(journal.Phase) > phaseOrder(step.phase) {
			continue
		}
		journal.Phase = step.phase
		if err := m.saveJournal(journal); err != nil {
			return err
		}
		cleanups, err := step.run(journal)
		deferredPhysicalRemovals = append(deferredPhysicalRemovals, cleanups...)
		if err != nil {
			if cleanupErr := tempdir.CleanupTemporaryDirectories(deferredPhysicalRemovals...); cleanupErr != nil {
				logrus.Errorf("Cleaning up after a failed storage cleanup phase %q: %v", step.phase, cleanupErr)
			}
			return err
		}
	}

	if phaseOrder(journal.Phase) < phaseOrder(phaseDriverDirs) {
		journal.Phase = phaseDriverDirs
		if err := m.saveJournal(journal); err != nil {
			return err
		}
	}
	if err := m.phaseDriverDirectories(journal, deferredPhysicalRemovals); err != nil {
		return err
	}

	m.fillReport(journal, report)
	return nil
}

func phaseOrder(phase cleanupPhase) int {
	switch phase {
	case phaseContainers:
		return 1
	case phaseImages:
		return 2
	case phaseMounts:
		return 3
	case phaseLayerIndex:
		return 4
	case phaseDriverDirs:
		return 5
	case phaseComplete:
		return 6
	}
	return 0
}

// collectAllImagesLocked returns images from the primary image store and
// every additional (read-only and read-write) image store. The primary
// stores must already be held for writing (as writeToAllStores does);
// additional stores are locked for reading briefly.
func (s *store) collectAllImagesLocked() ([]Image, error) {
	images, err := s.imageStore.Images()
	if err != nil {
		return nil, err
	}
	for _, store := range s.roImageStores {
		if err := store.startReading(); err != nil {
			return nil, err
		}
		storeImages, err := store.Images()
		store.stopReading()
		if err != nil {
			return nil, err
		}
		images = append(images, storeImages...)
	}
	return images, nil
}

// plan builds the reference snapshot and the cleanup intent. It runs while
// holding all three stores for writing, so the snapshot and the intent are
// computed against one coherent, locked view of the store.
func (m *cleanupManager) plan(options CleanupContainersOptions) (*cleanupJournal, error) {
	journal := &cleanupJournal{
		Version: cleanupJournalVersion,
		ID:      stringid.GenerateRandomID(),
		Created: time.Now().UTC(),
		Options: options,
		Phase:   phaseContainers,
		Snapshot: cleanupSnapshot{
			Containers:   map[string]cleanupContainerRef{},
			Images:       map[string]cleanupImageRef{},
			LayerParents: map[string]string{},
		},
		Layers: map[string]*cleanupLayerRecord{},
	}

	err := m.s.writeToAllStores(func(rlstore rwLayerStore) error {
		containers, err := m.s.containerStore.Containers()
		if err != nil {
			return err
		}
		images, err := m.s.collectAllImagesLocked()
		if err != nil {
			return err
		}
		layers, err := rlstore.Layers()
		if err != nil {
			return err
		}

		// Reference snapshot: the full graph at planning time.
		for _, c := range containers {
			journal.Snapshot.Containers[c.ID] = cleanupContainerRef{ImageID: c.ImageID, LayerID: c.LayerID}
		}
		for _, i := range images {
			journal.Snapshot.Images[i.ID] = cleanupImageRef{TopLayer: i.TopLayer, MappedTopLayers: slices.Clone(i.MappedTopLayers)}
		}
		for _, l := range layers {
			journal.Snapshot.LayerParents[l.ID] = l.Parent
		}

		// Select the intended containers.
		wanted := map[string]struct{}{}
		if options.AllContainers {
			for _, c := range containers {
				wanted[c.ID] = struct{}{}
			}
		} else {
			for _, id := range options.ContainerIDs {
				wanted[id] = struct{}{}
			}
		}

		// Image IDs that surviving (non-targeted plus locked) containers
		// keep references to.
		survivingContainerImages := map[string]struct{}{}
		// Layers used directly by surviving containers.
		survivingContainerLayers := map[string]struct{}{}
		// Image IDs referenced by scheduled containers.
		candidateImageIDs := map[string]struct{}{}

		for _, c := range containers {
			if _, ok := wanted[c.ID]; !ok {
				survivingContainerImages[c.ImageID] = struct{}{}
				survivingContainerLayers[c.LayerID] = struct{}{}
				continue
			}
			// A mounted (locked, in-use) container is preserved.
			if c.LayerID != "" && rlstore.Exists(c.LayerID) {
				if mounted, err := rlstore.Mounted(c.LayerID); err == nil && mounted > 0 {
					journal.Containers = append(journal.Containers, cleanupContainerRecord{
						ID:      c.ID,
						ImageID: c.ImageID,
						LayerID: c.LayerID,
						State:   statePreserved,
						Reason:  cleanupSkipMounted,
					})
					survivingContainerImages[c.ImageID] = struct{}{}
					survivingContainerLayers[c.LayerID] = struct{}{}
					continue
				}
			}
			journal.Containers = append(journal.Containers, cleanupContainerRecord{
				ID:      c.ID,
				ImageID: c.ImageID,
				LayerID: c.LayerID,
				State:   statePending,
			})
			if c.ImageID != "" {
				candidateImageIDs[c.ImageID] = struct{}{}
			}
			// The container's writable layer is a removal candidate.
			if c.LayerID != "" {
				m.addLayerCandidate(journal, rlstore, c.LayerID, "container:"+c.ID)
			}
		}
		// Unknown IDs requested by the caller are a no-op (cleanup is
		// idempotent), but an empty intent still completes normally.
		sort.Slice(journal.Containers, func(i, j int) bool {
			return journal.Containers[i].ID < journal.Containers[j].ID
		})

		if !options.RemoveUnreferencedImages {
			return nil
		}

		// Index the live images and layers.
		imagesByID := map[string]*Image{}
		for i := range images {
			imagesByID[images[i].ID] = &images[i]
		}
		layersByID := map[string]*Layer{}
		for i := range layers {
			layersByID[layers[i].ID] = &layers[i]
		}
		childrenByParent := map[string][]string{}
		for _, l := range layers {
			childrenByParent[l.Parent] = append(childrenByParent[l.Parent], l.ID)
		}

		// Images to remove: referenced by a scheduled container and not
		// referenced by any surviving container.
		removedImageIDs := map[string]struct{}{}
		for imageID := range candidateImageIDs {
			if _, keep := survivingContainerImages[imageID]; keep {
				continue
			}
			if _, ok := imagesByID[imageID]; !ok {
				// Image is already gone; its orphan layers (if any) are
				// still handled because the container layer was scheduled,
				// and image layers would have been scheduled by a prior
				// journal. Nothing to record here.
				continue
			}
			removedImageIDs[imageID] = struct{}{}
		}

		imageIDs := make([]string, 0, len(removedImageIDs))
		for id := range removedImageIDs {
			imageIDs = append(imageIDs, id)
		}
		sort.Strings(imageIDs)
		for _, id := range imageIDs {
			journal.Images = append(journal.Images, cleanupImageRecord{ID: id, State: statePending})
		}

		// Top layers of images that survive: their chains are shared and
		// must be preserved.
		survivingImageTopLayers := map[string]struct{}{}
		for i := range images {
			if _, removed := removedImageIDs[images[i].ID]; removed {
				continue
			}
			survivingImageTopLayers[images[i].TopLayer] = struct{}{}
			for _, mapped := range images[i].MappedTopLayers {
				survivingImageTopLayers[mapped] = struct{}{}
			}
		}

		// Build the layer removal set, mirroring the chain walk in
		// store.DeleteImage: from each removed image's top (and mapped
		// top) layers, walk parents while the layer is not used by a
		// surviving container/image and has no surviving children.
		layersToRemove := map[string]struct{}{}
		for id := range journal.Layers {
			layersToRemove[id] = struct{}{}
		}
		for _, imageID := range imageIDs {
			image := imagesByID[imageID]
			for _, mapped := range image.MappedTopLayers {
				layersToRemove[mapped] = struct{}{}
			}
			layerID := image.TopLayer
			for layerID != "" {
				if _, isContainerLayer := survivingContainerLayers[layerID]; isContainerLayer {
					break
				}
				if _, shared := survivingImageTopLayers[layerID]; shared {
					break
				}
				layer := layersByID[layerID]
				parent := ""
				if layer != nil {
					parent = layer.Parent
				}
				blocked := false
				for _, child := range childrenByParent[layerID] {
					if _, removing := layersToRemove[child]; removing {
						continue
					}
					blocked = true
					break
				}
				if blocked {
					break
				}
				layersToRemove[layerID] = struct{}{}
				layerID = parent
			}
		}
		for layerID := range layersToRemove {
			m.addLayerCandidate(journal, rlstore, layerID, "image")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return journal, nil
}

// addLayerCandidate records a layer removal candidate exactly once,
// capturing its parent and the creation generation that the deletion is
// fenced against.
func (m *cleanupManager) addLayerCandidate(journal *cleanupJournal, rlstore rwLayerStore, id, origin string) {
	if id == "" {
		return
	}
	if _, exists := journal.Layers[id]; exists {
		return
	}
	parent := ""
	if l, err := rlstore.Get(id); err == nil {
		parent = l.Parent
	}
	journal.Layers[id] = &cleanupLayerRecord{
		ID:         id,
		Parent:     parent,
		Origin:     origin,
		Generation: rlstore.layerGeneration(id),
		State:      statePending,
	}
}

// phaseContainers removes the container writable-tree/run directories and
// the container metadata records. The container's layer directory is NOT
// touched here: it becomes unreferenced and is collected in the later
// phases, with its dependency (this record) already gone durably.
func (m *cleanupManager) phaseContainers(journal *cleanupJournal) ([]tempdir.CleanupTempDirFunc, error) {
	err := m.s.writeToAllStores(func(rlstore rwLayerStore) error {
		for i := range journal.Containers {
			record := &journal.Containers[i]
			if record.State != statePending {
				continue
			}
			container, err := m.s.containerStore.Get(record.ID)
			if errors.Is(err, ErrContainerUnknown) {
				// Already committed as removed by an interrupted run.
				record.State = stateDone
				return m.saveJournal(journal)
			}
			if err != nil {
				return err
			}
			// Re-check the lock at execution time too: a container that
			// got mounted after planning is preserved.
			if container.LayerID != "" && rlstore.Exists(container.LayerID) {
				if mounted, err := rlstore.Mounted(container.LayerID); err == nil && mounted > 0 {
					record.State = statePreserved
					record.Reason = cleanupSkipMounted
					if err := m.saveJournal(journal); err != nil {
						return err
					}
					continue
				}
			}
			record.ImageID = container.ImageID
			record.LayerID = container.LayerID

			middleDir := m.s.graphDriverName + "-containers"
			var wg errgroup.Group
			wg.Go(func() error {
				return system.EnsureRemoveAll(filepath.Join(m.s.GraphRoot(), middleDir, container.ID))
			})
			wg.Go(func() error {
				return system.EnsureRemoveAll(filepath.Join(m.s.RunRoot(), middleDir, container.ID))
			})
			if err := wg.Wait(); err != nil {
				return err
			}
			// Delete the container record (and its big-data directory).
			// If the layer still exists, the later phases collect it; the
			// journal retains the binding so the layer can never be lost
			// track of.
			if err := m.s.containerStore.Delete(container.ID); err != nil && !errors.Is(err, ErrContainerUnknown) {
				return err
			}
			record.State = stateDone
			if err := m.saveJournal(journal); err != nil {
				return err
			}
		}
		return nil
	})
	return nil, err
}

// phaseImages removes image metadata records once no live container refers
// to them. Layer chain removal follows in later phases, so readers can
// never observe an image pointing at a missing layer.
func (m *cleanupManager) phaseImages(journal *cleanupJournal) ([]tempdir.CleanupTempDirFunc, error) {
	err := m.s.writeToAllStores(func(rlstore rwLayerStore) error {
		for i := range journal.Images {
			record := &journal.Images[i]
			if record.State != statePending {
				continue
			}
			// Revalidate against the current container set: an image
			// created or retained concurrently must be preserved.
			containers, err := m.s.containerStore.Containers()
			if err != nil {
				return err
			}
			referenced := false
			for _, c := range containers {
				if c.ImageID == record.ID {
					referenced = true
					break
				}
			}
			if referenced {
				record.State = statePreserved
				record.Reason = "image is used by a container"
				if err := m.saveJournal(journal); err != nil {
					return err
				}
				continue
			}

			found := false
			for _, is := range m.s.rwImageStores {
				if is != m.s.imageStore {
					if err := is.startWriting(); err != nil {
						return err
					}
					defer is.stopWriting()
				}
				if !is.Exists(record.ID) {
					continue
				}
				found = true
				if err := is.Delete(record.ID); err != nil {
					return fmt.Errorf("deleting image %q: %w", record.ID, err)
				}
			}
			if !found {
				// Committed by an interrupted run.
				logrus.Debugf("Image %q was already removed by an earlier cleanup attempt", record.ID)
			}
			record.State = stateDone
			if err := m.saveJournal(journal); err != nil {
				return err
			}
		}
		return nil
	})
	return nil, err
}

// phaseMounts tears down mounts of layers scheduled for removal. A layer
// that cannot be unmounted is preserved for this run.
func (m *cleanupManager) phaseMounts(journal *cleanupJournal) ([]tempdir.CleanupTempDirFunc, error) {
	err := m.s.writeToAllStores(func(rlstore rwLayerStore) error {
		ids := m.orderedLayerIDs(journal)
		for _, id := range ids {
			record := journal.Layers[id]
			if record.State == statePreserved || record.Unmounted {
				continue
			}
			if !rlstore.Exists(id) {
				record.Unmounted = true
				if err := m.saveJournal(journal); err != nil {
					return err
				}
				continue
			}
			// Generation fence: never manipulate a layer ID that was
			// re-created after this cleanup was scheduled.
			if rlstore.layerGeneration(id) != record.Generation {
				record.State = statePreserved
				record.Reason = "layer was re-created in a newer generation"
				if err := m.saveJournal(journal); err != nil {
					return err
				}
				continue
			}
			for {
				_, err := rlstore.unmount(id, false, true)
				if errors.Is(err, ErrLayerNotMounted) {
					break
				}
				if err != nil {
					record.State = statePreserved
					record.Reason = fmt.Sprintf("layer is mounted and cannot be unmounted: %v", err)
					break
				}
			}
			if record.State == statePreserved {
				if err := m.saveJournal(journal); err != nil {
					return err
				}
				continue
			}
			record.Unmounted = true
			if err := m.saveJournal(journal); err != nil {
				return err
			}
		}
		return nil
	})
	return nil, err
}

// phaseLayerIndex removes the layer metadata records (and stages driver
// directory removal via the layer store's existing deferred-delete
// machinery) for every candidate that is still unreferenced at commit
// time. Layers are removed children-before-parents over as many fixup
// passes as necessary.
func (m *cleanupManager) phaseLayerIndex(journal *cleanupJournal) ([]tempdir.CleanupTempDirFunc, error) {
	var cleanups []tempdir.CleanupTempDirFunc
	err := m.s.writeToAllStores(func(rlstore rwLayerStore) error {
		// Live reference sets for revalidation.
		containers, err := m.s.containerStore.Containers()
		if err != nil {
			return err
		}
		containerLayers := map[string]struct{}{}
		for _, c := range containers {
			containerLayers[c.LayerID] = struct{}{}
		}
		images, err := m.s.collectAllImagesLocked()
		if err != nil {
			return err
		}
		imageLayers := map[string]struct{}{}
		for _, i := range images {
			imageLayers[i.TopLayer] = struct{}{}
			for _, mapped := range i.MappedTopLayers {
				imageLayers[mapped] = struct{}{}
			}
		}

		progress := true
		for progress {
			progress = false
			// Re-read the live layer graph each pass: deferredDelete in
			// a previous pass changes it.
			allLayers, err := rlstore.Layers()
			if err != nil {
				return err
			}
			childrenByParent := map[string][]string{}
			for _, child := range allLayers {
				childrenByParent[child.Parent] = append(childrenByParent[child.Parent], child.ID)
			}
			ids := m.orderedLayerIDs(journal)
			for _, id := range ids {
				record := journal.Layers[id]
				if record.State == statePreserved || record.IndexRemoved {
					continue
				}

				if !rlstore.Exists(id) {
					// Index removal was already committed by an
					// interrupted run. If the generation advanced, the ID
					// was re-created and the new generation is preserved;
					// otherwise the earlier deletion simply continues.
					if rlstore.layerGeneration(id) != record.Generation {
						record.State = statePreserved
						record.Reason = "layer was re-created in a newer generation"
					} else {
						record.IndexRemoved = true
					}
					if err := m.saveJournal(journal); err != nil {
						return err
					}
					progress = true
					continue
				}

				// Fence against re-creation.
				if rlstore.layerGeneration(id) != record.Generation {
					record.State = statePreserved
					record.Reason = "layer was re-created in a newer generation"
					if err := m.saveJournal(journal); err != nil {
						return err
					}
					continue
				}
				// Preserve anything still referenced by a live container
				// or image.
				if _, used := containerLayers[id]; used {
					record.State = statePreserved
					record.Reason = "layer is used by a container"
					if err := m.saveJournal(journal); err != nil {
						return err
					}
					continue
				}
				if _, used := imageLayers[id]; used {
					record.State = statePreserved
					record.Reason = "layer is used by an image"
					if err := m.saveJournal(journal); err != nil {
						return err
					}
					continue
				}
				// Children go first. A surviving child (not a candidate,
				// or preserved/removed-elsewhere) blocks this layer.
				blocked := false
				for _, childID := range childrenByParent[id] {
					childRecord, isCandidate := journal.Layers[childID]
					if !isCandidate || childRecord.State == statePreserved || !childRecord.IndexRemoved {
						blocked = true
						break
					}
				}
				if blocked {
					continue
				}

				// Commit: mark incomplete durably, stage driver-directory
				// removal, drop the index entry and persist layer metadata
				// atomically. Physical removal runs after the locks are
				// released (or, after a crash, via driver/temp-dir recovery
				// and GarbageCollect).
				layerCleanups, err := rlstore.deferredDelete(id)
				cleanups = append(cleanups, layerCleanups...)
				if err != nil && !errors.Is(err, ErrLayerUnknown) {
					return fmt.Errorf("deleting layer %q: %w", id, err)
				}
				record.IndexRemoved = true
				if err := m.saveJournal(journal); err != nil {
					return err
				}
				progress = true
			}
		}

		// Layers still pending have surviving references that the live
		// revalidation found; preserve them for this run.
		for _, id := range m.orderedLayerIDs(journal) {
			record := journal.Layers[id]
			if record.State == statePreserved || record.IndexRemoved {
				continue
			}
			record.State = statePreserved
			record.Reason = "layer is still referenced"
			if err := m.saveJournal(journal); err != nil {
				return err
			}
		}
		return nil
	})
	return cleanups, err
}

// phaseDriverDirectories finishes the physical removals: cleanup functions
// returned by deferred-delete are run outside the store locks for this
// process, and GarbageCollect reaps driver directories orphaned by a
// process that died before it could run them.
func (m *cleanupManager) phaseDriverDirectories(journal *cleanupJournal, cleanups []tempdir.CleanupTempDirFunc) error {
	// Slow disk I/O happens here; do it without holding the metadata
	// locks, exactly like the ordinary Delete* methods do.
	if err := tempdir.CleanupTemporaryDirectories(cleanups...); err != nil {
		return err
	}
	_, err := writeToLayerStore(m.s, func(rlstore rwLayerStore) (struct{}, error) {
		// Reap leftovers of a process killed between staging the driver
		// directory removal and executing the cleanup.
		if err := rlstore.GarbageCollect(); err != nil {
			return struct{}{}, err
		}
		for _, id := range m.orderedLayerIDs(journal) {
			record := journal.Layers[id]
			if record.State == statePreserved {
				continue
			}
			if !record.IndexRemoved {
				return struct{}{}, fmt.Errorf("internal error: layer %q reached driver-directory phase without index removal", id)
			}
			if rlstore.Exists(id) {
				// The ID came back (newer generation): preserve it and
				// never remove its (new) driver directory.
				if rlstore.layerGeneration(id) != record.Generation {
					record.State = statePreserved
					record.Reason = "layer was re-created in a newer generation"
					continue
				}
				return struct{}{}, fmt.Errorf("layer %q index was removed but the layer exists again", id)
			}
			record.DriverDirRemoved = true
			record.State = stateDone
		}
		journal.Phase = phaseComplete
		return struct{}{}, m.saveJournal(journal)
	})
	if err != nil {
		return err
	}
	return m.removeJournal()
}

// orderedLayerIDs returns layer IDs in a deterministic order grouped so
// that chains tend to be processed top-down.
func (m *cleanupManager) orderedLayerIDs(journal *cleanupJournal) []string {
	ids := make([]string, 0, len(journal.Layers))
	for id := range journal.Layers {
		ids = append(ids, id)
	}
	// Parents after their children: sort by (parent present in set, id)
	// approximation; the commit loop itself enforces the real order.
	sort.Strings(ids)
	return ids
}

func (m *cleanupManager) fillReport(journal *cleanupJournal, report *CleanupReport) {
	if report == nil {
		return
	}
	for _, c := range journal.Containers {
		switch c.State {
		case stateDone:
			report.ContainersRemoved = append(report.ContainersRemoved, c.ID)
		case statePreserved:
			report.ContainersPreserved = append(report.ContainersPreserved, c.ID)
		case statePending:
			// Dry-run intent reports pending items as removals.
			if report.DryRun {
				report.ContainersRemoved = append(report.ContainersRemoved, c.ID)
			}
		}
	}
	for _, i := range journal.Images {
		switch i.State {
		case stateDone:
			report.ImagesRemoved = append(report.ImagesRemoved, i.ID)
		case statePreserved:
			report.ImagesPreserved = append(report.ImagesPreserved, i.ID)
		case statePending:
			if report.DryRun {
				report.ImagesRemoved = append(report.ImagesRemoved, i.ID)
			}
		}
	}
	for _, l := range journal.Layers {
		switch l.State {
		case stateDone:
			report.LayersRemoved = append(report.LayersRemoved, l.ID)
		case statePreserved:
			report.LayersPreserved = append(report.LayersPreserved, l.ID)
		case statePending:
			if report.DryRun {
				report.LayersRemoved = append(report.LayersRemoved, l.ID)
			}
		}
	}
	sort.Strings(report.ContainersRemoved)
	sort.Strings(report.ContainersPreserved)
	sort.Strings(report.ImagesRemoved)
	sort.Strings(report.ImagesPreserved)
	sort.Strings(report.LayersRemoved)
	sort.Strings(report.LayersPreserved)
}
