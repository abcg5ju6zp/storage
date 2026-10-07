package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/storage/pkg/ioutils"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/containers/storage/pkg/stringid"
	"github.com/sirupsen/logrus"
)

// Statistics for the layer and image indexes are published in generations.
//
// A refresh never modifies the live in-memory indexes or the on-disk index
// files in place.  It first collects all statistics and validates the
// layer/image relationships, materializes the resulting indexes as a
// candidate snapshot on disk, and only then installs the candidate files and
// advances the lock files.  A single, atomically-replaced marker file names
// the generation that is fully published.
//
// On-disk layout (in the primary layer store directory):
//
//   - layers.json, images.json (the image index lives in the image store
//     directory) are always complete indexes of the last fully published
//     generation.
//   - stats-snapshots/<generation>/manifest.json describes exactly one
//     prepared candidate and how it is to be published.
//   - .stats-candidate-<generation>-{layers,images}.json are the fully
//     written and synced candidate index files, each located in the same
//     directory as its target so that publication is a rename(2).
//   - stats-committed.json is the publication marker and the only
//     statistics state that status queries are allowed to read.
//
// If the process disappears at any point, the next startup either completes
// the unique candidate (if every on-disk index is still either the recorded
// base or the candidate content) or discards it (if a newer unrelated write
// happened meanwhile).  Index files are never left mixing generations.
const (
	statsSnapshotDirName      = "stats-snapshots"
	statsCommittedFileName    = "stats-committed.json"
	statsSnapshotManifestName = "manifest.json"
	statsSnapshotStateReady   = "prepared"
	statsSnapshotVersion      = 1
	statsCandidateTempPrefix  = ".stats-candidate-"

	statsTargetLayers = "layers"
	statsTargetImages = "images"
)

var (
	// errStatisticsSnapshotInvalid indicates that a candidate snapshot is
	// incomplete or corrupt on disk and can only be discarded.
	errStatisticsSnapshotInvalid = errors.New("statistics candidate snapshot is incomplete or corrupt")
	// errStatisticsSnapshotSuperseded indicates that the on-disk indexes
	// were changed by an unrelated writer after the candidate was prepared,
	// so the candidate must not be published.
	errStatisticsSnapshotSuperseded = errors.New("statistics candidate snapshot is superseded by a newer write")
	// errStatisticsVerification indicates that the collected statistics fail
	// reference or size consistency checks, so no snapshot is published.
	errStatisticsVerification = errors.New("statistics verification failed")
)

// FilesystemStatistics reports capacity of the filesystem backing the store,
// as observed when a statistics generation was collected.
type FilesystemStatistics struct {
	// TotalBytes is the total capacity of the filesystem in bytes.
	TotalBytes int64 `json:"total-bytes,omitempty"`
	// FreeBytes is the number of unallocated bytes in the filesystem.
	FreeBytes int64 `json:"free-bytes,omitempty"`
	// AvailableBytes is the number of bytes available to an unprivileged
	// caller (FreeBytes minus the space reserved for the superuser).
	AvailableBytes int64 `json:"available-bytes,omitempty"`
	// Inodes is the total number of inodes in the filesystem.
	Inodes int64 `json:"inodes,omitempty"`
	// InodesAvailable is the number of free inodes in the filesystem.
	InodesAvailable int64 `json:"inodes-available,omitempty"`
}

// LayerStatistics is the collected capacity information for one layer in a
// committed statistics generation.
type LayerStatistics struct {
	// ID is the ID of the layer.
	ID string `json:"id"`
	// Size is the on-disk size, in bytes, of the layer's writable
	// directory, as reported by the graph driver.
	Size int64 `json:"size"`
	// Inodes is the number of inodes used by the layer.
	Inodes int64 `json:"inodes"`
	// DiffSize is the size, in bytes, of the tarstream describing the
	// layer relative to its parent, as reported by the graph driver.
	DiffSize int64 `json:"diff-size"`
	// QuotaEnabled is true if Size and Inodes were tracked through
	// filesystem quota rather than a directory walk.
	QuotaEnabled bool `json:"quota-enabled,omitempty"`
}

// LayerStatisticsReport describes one statistics generation.  The same
// structure is persisted in the committed-generation marker.
type LayerStatisticsReport struct {
	// Version is the on-disk format version of the report.
	Version int `json:"version"`
	// Generation is the unique identifier of the statistics generation.
	Generation string `json:"generation"`
	// CreatedAt is when the statistics of this generation were collected.
	CreatedAt time.Time `json:"created-at"`
	// Driver is the name of the graph driver that produced the statistics.
	Driver string `json:"driver"`
	// Changed reports whether the refresh produced a new generation.  A
	// repeated refresh that observes identical statistics is a no-op and
	// reports the previous generation with Changed set to false.
	Changed bool `json:"changed,omitempty"`
	// Filesystem is the capacity of the filesystem hosting the store.
	Filesystem FilesystemStatistics `json:"filesystem"`
	// Layers is the per-layer statistics of the generation.
	Layers []LayerStatistics `json:"layers"`
}

// statsSnapshotTarget describes one index file participating in a candidate
// generation.
type statsSnapshotTarget struct {
	// Kind is statsTargetLayers or statsTargetImages.
	Kind string `json:"kind"`
	// Path is the absolute path of the committed index file.
	Path string `json:"path"`
	// LockPath is the lock file that protects the target index.
	LockPath string `json:"lock-path"`
	// TempName is the base name (in Path's directory) of the fully
	// written, synced candidate payload.
	TempName string `json:"temp-name"`
	// Size is the candidate payload length in bytes.
	Size int64 `json:"size"`
	// SHA256 is the hex SHA-256 digest of the candidate payload.
	SHA256 string `json:"sha256"`
	// BasePresent records whether the target existed when the candidate
	// was prepared.
	BasePresent bool `json:"base-present,omitempty"`
	// BaseSize is the length of the payload the candidate is replacing.
	BaseSize int64 `json:"base-size"`
	// BaseSHA256 is the hex SHA-256 digest of the payload the candidate
	// is replacing.
	BaseSHA256 string `json:"base-sha256,omitempty"`
}

// statsSnapshotManifest is the on-disk description of a prepared candidate
// generation.  The existence of a complete manifest means that all candidate
// payloads have been written and synced; publication is then a sequence of
// renames and lock advances.
type statsSnapshotManifest struct {
	// Version is the on-disk format version, currently statsSnapshotVersion.
	Version int `json:"version"`
	// State is the publication state, currently always statsSnapshotStateReady:
	// a candidate is only described by a manifest once it is fully prepared.
	State string `json:"state"`
	// Generation is the unique identifier of the candidate generation.
	Generation string `json:"generation"`
	// CreatedAt is when the candidate was prepared.
	CreatedAt time.Time `json:"created-at"`
	// Driver is the graph driver the candidate statistics were collected with.
	Driver string `json:"driver"`
	// Layers is the layer index target.
	Layers statsSnapshotTarget `json:"layers"`
	// Images is the image index target.
	Images statsSnapshotTarget `json:"images"`
	// Filesystem is the filesystem capacity observed for the generation.
	Filesystem FilesystemStatistics `json:"filesystem"`
	// Stats is the per-layer statistics recorded by the candidate.
	Stats []LayerStatistics `json:"stats"`
}

// projectQuotaDriver is implemented by graph drivers that track disk usage
// through filesystem project quota.  Drivers that do not implement it are
// assumed to obtain usage by walking the layer directory.
type projectQuotaDriver interface {
	ProjectQuotaSupported() bool
}

// statisticsWriteLock is the subset of the lock file API used while
// publishing a generation.
type statisticsWriteLock interface {
	RecordWrite() (lockfile.LastWrite, error)
}

// statisticsManager runs one statistics refresh against the primary
// read-write layer and image stores.  All of its methods except the startup
// recovery helper require the caller to hold (in locking order) the primary
// layer store for writing, the additional layer stores for reading, and the
// primary image store for writing.
type statisticsManager struct {
	store    *store
	rls      *layerStore
	roLayers []roLayerStore
	img      *imageStore

	layerDir   string
	imageDir   string
	layersPath string
	imagesPath string

	quotaSupported bool
}

func newStatisticsManager(s *store, rls *layerStore, roLayers []roLayerStore, img *imageStore) *statisticsManager {
	return &statisticsManager{
		store:      s,
		rls:        rls,
		roLayers:   roLayers,
		img:        img,
		layerDir:   rls.layerdir,
		imageDir:   img.dir,
		layersPath: rls.jsonPath[0],
		imagesPath: img.imagespath(),
	}
}

func (m *statisticsManager) snapshotRoot() string {
	return filepath.Join(m.layerDir, statsSnapshotDirName)
}

func (m *statisticsManager) snapshotDir(generation string) string {
	return filepath.Join(m.snapshotRoot(), generation)
}

// layerExistsAnywhere reports whether id is known to the primary layer
// store or any additional read-only layer store.
func (m *statisticsManager) layerExistsAnywhere(id string) bool {
	if _, ok := m.rls.lookup(id); ok {
		return true
	}
	for _, ro := range m.roLayers {
		if ro.Exists(id) {
			return true
		}
	}
	return false
}

// collectedLayerStatistics holds the freshly observed statistics for a single
// layer while a candidate is being built.
type collectedLayerStatistics struct {
	layer        *Layer
	size         int64
	inodes       int64
	diffSize     int64
	quotaEnabled bool
}

func (m *statisticsManager) quotaInUse() bool {
	if qd, ok := m.store.graphDriver.(projectQuotaDriver); ok {
		return qd.ProjectQuotaSupported()
	}
	return false
}

// collect gathers driver usage, diff size and quota information for every
// stable layer and verifies references and size consistency.  It never
// modifies any state: a failure means that no candidate must be published.
func (m *statisticsManager) collect() ([]collectedLayerStatistics, error) {
	m.quotaSupported = m.quotaInUse()

	collected := make([]collectedLayerStatistics, 0, len(m.rls.layers))
	observed := make(map[string]struct{})
	var errs []error

	// Layers that are not present in the driver (for example layers that
	// were created but never populated) have no observable statistics;
	// they are left untouched rather than invalidating the generation.
	for _, layer := range m.rls.layers {
		if layer.location != stableLayerLocation {
			continue
		}
		if !m.rls.driver.Exists(layer.ID) {
			continue
		}

		usage, err := m.rls.driver.ReadWriteDiskUsage(layer.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("collecting disk usage of layer %q: %w", layer.ID, err))
			continue
		}
		if usage == nil || usage.Size < 0 || usage.InodeCount < 0 {
			errs = append(errs, fmt.Errorf("collecting disk usage of layer %q: driver reported invalid usage %#v", layer.ID, usage))
			continue
		}

		diffSize, err := m.rls.DiffSize(layer.Parent, layer.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("collecting diff size of layer %q: %w", layer.ID, err))
			continue
		}

		// Size sanity: drivers must never report negative capacities.  Such a
		// value can only be a broken observation and must not be published.
		if diffSize < 0 {
			errs = append(errs, fmt.Errorf("collecting diff size of layer %q: driver reported invalid size %d", layer.ID, diffSize))
			continue
		}

		// Every layer chain must resolve: a parent that is known to none of
		// the layer stores would produce an inconsistent layer index.
		if layer.Parent != "" && !m.layerExistsAnywhere(layer.Parent) {
			errs = append(errs, fmt.Errorf("layer %s: parent layer %q is unknown: %w", layer.ID, layer.Parent, ErrParentUnknown))
		}

		collected = append(collected, collectedLayerStatistics{
			layer:        layer,
			size:         usage.Size,
			inodes:       usage.InodeCount,
			diffSize:     diffSize,
			quotaEnabled: m.quotaSupported,
		})
		observed[layer.ID] = struct{}{}
	}

	// Every image in the primary image index must reference layers that the
	// layer stores can resolve, and a top layer owned by the primary layer
	// store must carry statistics in this very generation, so that a
	// published image index and a published layer index always agree on the
	// layer graph and the reported capacities.
	for _, image := range m.img.images {
		refs := make([]string, 0, len(image.MappedTopLayers)+1)
		refs = append(refs, image.MappedTopLayers...)
		if image.TopLayer != "" {
			refs = append(refs, image.TopLayer)
		}
		for _, ref := range refs {
			if !m.layerExistsAnywhere(ref) {
				errs = append(errs, fmt.Errorf("image %s: references unknown layer %q: %w", image.ID, ref, ErrLayerUnknown))
				continue
			}
			if primary, ok := m.rls.lookup(ref); ok && primary.location == stableLayerLocation {
				if _, ok := observed[ref]; !ok {
					errs = append(errs, fmt.Errorf("image %s: top layer %q has no statistics in this generation: %w",
						image.ID, ref, errStatisticsVerification))
				}
			}
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("%w:\n%w", errStatisticsVerification, errors.Join(errs...))
	}
	return collected, nil
}

// statisticsUnchanged reports whether every stable layer still belongs to the
// committed generation and carries exactly the statistics that would be
// published.  Repeated refreshes while nothing changed must not advance the
// generation, so that consumers (for example the garbage collector) do not
// observe repeated new generations.
func (m *statisticsManager) statisticsUnchanged(collected []collectedLayerStatistics, committedGeneration string) bool {
	for _, layer := range m.rls.layers {
		if layer.location != stableLayerLocation {
			continue
		}
		if layer.StatsGeneration != committedGeneration {
			return false
		}
	}
	for _, c := range collected {
		l := c.layer
		if l.StatsSize != c.size || l.StatsInodes != c.inodes ||
			l.StatsDiffSize != c.diffSize || l.StatsQuotaEnabled != c.quotaEnabled {
			return false
		}
	}
	// Images are published in the same generation as layer statistics. A newly
	// created image starts without a generation marker, so it must force a
	// refresh instead of being hidden by the layer-only comparison above.
	for _, image := range m.img.images {
		if image.StatsGeneration != committedGeneration {
			return false
		}
	}
	return true
}

// buildCandidate applies the collected statistics to deep copies of the live
// indexes and returns the payloads that would replace layers.json and
// images.json, plus the per-layer statistics in index order.  The live
// indexes are not modified here.
func (m *statisticsManager) buildCandidate(generation string, at time.Time, collected []collectedLayerStatistics) (layersPayload, imagesPayload []byte, stats []LayerStatistics, err error) {
	valuesByID := make(map[string]collectedLayerStatistics, len(collected))
	stats = make([]LayerStatistics, 0, len(collected))
	for _, c := range collected {
		valuesByID[c.layer.ID] = c
		stats = append(stats, LayerStatistics{
			ID:           c.layer.ID,
			Size:         c.size,
			Inodes:       c.inodes,
			DiffSize:     c.diffSize,
			QuotaEnabled: c.quotaEnabled,
		})
	}

	candidateLayers := make([]*Layer, 0, len(m.rls.layers))
	for _, layer := range m.rls.layers {
		candidate := copyLayer(layer)
		if layer.location == stableLayerLocation {
			if c, ok := valuesByID[layer.ID]; ok {
				candidate.StatsGeneration = generation
				candidate.StatsAt = at
				candidate.StatsSize = c.size
				candidate.StatsInodes = c.inodes
				candidate.StatsDiffSize = c.diffSize
				candidate.StatsQuotaEnabled = c.quotaEnabled
			}
		}
		candidateLayers = append(candidateLayers, candidate)
	}

	// Only the stable location is published by a statistics refresh;
	// volatile and image-store-local layer indexes are left untouched.
	stableLayers := make([]*Layer, 0, len(candidateLayers))
	for _, layer := range candidateLayers {
		if layer.location == stableLayerLocation {
			stableLayers = append(stableLayers, layer)
		}
	}
	layersPayload, err = json.Marshal(&stableLayers)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding candidate layer index: %w", err)
	}

	candidateImages := make([]*Image, 0, len(m.img.images))
	for _, image := range m.img.images {
		candidate := copyImage(image)
		candidate.StatsGeneration = generation
		candidateImages = append(candidateImages, candidate)
	}
	imagesPayload, err = json.Marshal(&candidateImages)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding candidate image index: %w", err)
	}

	return layersPayload, imagesPayload, stats, nil
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeSyncedFile creates or truncates path, writes data, and flushes it to
// stable storage.  The file is created in its final directory with its final
// name (candidate names never shadow committed files), so a crash can leave
// at most an additional candidate file, never a partially written index.
func writeSyncedFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = fmt.Errorf("short write to %q: %d of %d bytes", path, n, len(data))
	}
	// Sync failures are only acceptable if the write already failed.
	if syncErr := f.Sync(); syncErr != nil && err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

// readBasePayload returns the current contents of an index file, recording
// whether the file existed.
func readBasePayload(path string) (present bool, data []byte, err error) {
	data, err = os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, data, nil
}

// prepare writes and fsyncs the candidate payloads and its manifest. When it
// returns successfully the candidate is durable; nothing committed has been
// touched yet.
func (m *statisticsManager) prepare(generation string, at time.Time, fsStat FilesystemStatistics, layersPayload, imagesPayload []byte, stats []LayerStatistics) (_ *statsSnapshotManifest, retErr error) {
	manifest := &statsSnapshotManifest{
		Version:    statsSnapshotVersion,
		State:      statsSnapshotStateReady,
		Generation: generation,
		CreatedAt:  at,
		Driver:     m.store.graphDriverName,
		Filesystem: fsStat,
		Stats:      stats,
	}

	// Candidate payloads live next to their targets so publication is a
	// same-filesystem rename.
	layersTemp := filepath.Join(m.layerDir, statsCandidateTempPrefix+generation+"-layers.json")
	imagesTemp := filepath.Join(m.imageDir, statsCandidateTempPrefix+generation+"-images.json")

	targets := []struct {
		kind, path, temp, lockPath string
		payload                    []byte
		dst                        *statsSnapshotTarget
	}{
		{statsTargetLayers, m.layersPath, layersTemp, filepath.Join(m.layerDir, "layers.lock"), layersPayload, &manifest.Layers},
		{statsTargetImages, m.imagesPath, imagesTemp, filepath.Join(m.imageDir, "images.lock"), imagesPayload, &manifest.Images},
	}

	snapshotDir := m.snapshotDir(generation)
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating statistics snapshot directory %q: %w", snapshotDir, err)
	}
	defer func() {
		if retErr != nil {
			// Preparation failed: no publication is possible, so remove all
			// traces of the candidate.  The committed indexes are untouched.
			if err := m.discardCandidate(manifest); err != nil {
				logrus.Errorf("Cleaning up failed statistics snapshot %q: %v", generation, err)
			}
		}
	}()

	for _, t := range targets {
		if err := writeSyncedFile(t.temp, t.payload, 0o600); err != nil {
			return nil, fmt.Errorf("writing candidate %s index: %w", t.kind, err)
		}
		present, base, err := readBasePayload(t.path)
		if err != nil {
			return nil, fmt.Errorf("reading current %s index: %w", t.kind, err)
		}
		t.dst.Kind = t.kind
		t.dst.Path = t.path
		t.dst.LockPath = t.lockPath
		t.dst.TempName = filepath.Base(t.temp)
		t.dst.Size = int64(len(t.payload))
		t.dst.SHA256 = checksum(t.payload)
		t.dst.BasePresent = present
		t.dst.BaseSize = int64(len(base))
		if present {
			t.dst.BaseSHA256 = checksum(base)
		}
	}

	// The manifest is the "candidate is complete" signal, so publish it
	// atomically: recovery code is allowed to assume that whenever a
	// manifest exists, all payloads are already fully written and synced.
	manifestPath := filepath.Join(snapshotDir, statsSnapshotManifestName)
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encoding statistics snapshot manifest: %w", err)
	}
	manifestTemp := filepath.Join(snapshotDir, ".manifest.tmp")
	if err := writeSyncedFile(manifestTemp, manifestData, 0o600); err != nil {
		return nil, fmt.Errorf("writing statistics snapshot manifest: %w", err)
	}
	if err := os.Rename(manifestTemp, manifestPath); err != nil {
		return nil, fmt.Errorf("publishing statistics snapshot manifest: %w", err)
	}
	if err := syncDirectory(snapshotDir); err != nil {
		return nil, fmt.Errorf("syncing statistics snapshot directory: %w", err)
	}
	return manifest, nil
}

// targetPublicationState classifies an on-disk target relative to the
// candidate.  It returns one of "installed" (target already holds the
// candidate payload), "base" (target still holds the recorded base), or
// "other" (target holds neither, meaning a newer unrelated write happened).
func targetPublicationState(t *statsSnapshotTarget) (state string, err error) {
	data, readErr := os.ReadFile(t.Path)
	if readErr != nil {
		if !os.IsNotExist(readErr) {
			return "", readErr
		}
		data = nil
	}
	if int64(len(data)) == t.Size && checksum(data) == t.SHA256 {
		return "installed", nil
	}
	if !t.BasePresent {
		// The target did not exist when the candidate was prepared.
		if len(data) == 0 && !fileExists(t.Path) {
			return "base", nil
		}
		return "other", nil
	}
	if int64(len(data)) == t.BaseSize && checksum(data) == t.BaseSHA256 {
		return "base", nil
	}
	return "other", nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// publishPrepared installs a fully prepared candidate generation.
//
// It is the single publication path used both by the live refresh and by
// startup recovery, so resuming a partially completed publication is exactly
// the same operation as completing a fresh one.
//
// Targets are published in a fixed order (layer index, then image index);
// the lock file of each target is advanced immediately before its index file
// is renamed, preserving the invariant that a lock write always precedes the
// index write it describes.  The caller must hold both write locks.
func publishPrepared(manifest *statsSnapshotManifest, layersLock, imagesLock statisticsWriteLock) (layersLastWrite, imagesLastWrite lockfile.LastWrite, layersRecorded, imagesRecorded bool, retErr error) {
	targets := []struct {
		t        *statsSnapshotTarget
		lock     statisticsWriteLock
		lw       *lockfile.LastWrite
		recorded *bool
	}{
		{&manifest.Layers, layersLock, &layersLastWrite, &layersRecorded},
		{&manifest.Images, imagesLock, &imagesLastWrite, &imagesRecorded},
	}

	for _, target := range targets {
		state, err := targetPublicationState(target.t)
		if err != nil {
			return layersLastWrite, imagesLastWrite, false, false, fmt.Errorf("inspecting %s index during statistics publication: %w", target.t.Kind, err)
		}
		switch state {
		case "installed":
			// Already published in an earlier (interrupted) run; resuming
			// must not rewrite or advance the lock again.
			continue
		case "other":
			return layersLastWrite, imagesLastWrite, false, false, fmt.Errorf("not publishing statistics snapshot %q: %s index %q: %w",
				manifest.Generation, target.t.Kind, target.t.Path, errStatisticsSnapshotSuperseded)
		}

		lw, err := target.lock.RecordWrite()
		if err != nil {
			return layersLastWrite, imagesLastWrite, false, false, fmt.Errorf("recording write of %s index for statistics snapshot %q: %w",
				target.t.Kind, manifest.Generation, err)
		}
		*target.lw = lw
		*target.recorded = true

		tempPath := filepath.Join(filepath.Dir(target.t.Path), target.t.TempName)
		if err := os.Rename(tempPath, target.t.Path); err != nil {
			return layersLastWrite, imagesLastWrite, layersRecorded, imagesRecorded, fmt.Errorf("publishing %s index of statistics snapshot %q: %w",
				target.t.Kind, manifest.Generation, err)
		}
		if err := syncDirectory(filepath.Dir(target.t.Path)); err != nil {
			return layersLastWrite, imagesLastWrite, layersRecorded, imagesRecorded, fmt.Errorf("syncing %s index directory: %w", target.t.Kind, err)
		}
	}

	// Both indexes now carry the generation.  The atomic replacement of
	// this marker is the single publication point; only generations named
	// here are considered committed.
	report := reportFromManifest(manifest)
	committedData, err := json.Marshal(report)
	if err != nil {
		return layersLastWrite, imagesLastWrite, layersRecorded, imagesRecorded, fmt.Errorf("encoding statistics commit marker: %w", err)
	}
	committedPath := filepath.Join(filepath.Dir(manifest.Layers.Path), statsCommittedFileName)
	if err := ioutils.AtomicWriteFile(committedPath, committedData, 0o600); err != nil {
		return layersLastWrite, imagesLastWrite, layersRecorded, imagesRecorded, fmt.Errorf("writing statistics commit marker: %w", err)
	}
	if err := syncDirectory(filepath.Dir(committedPath)); err != nil {
		return layersLastWrite, imagesLastWrite, layersRecorded, imagesRecorded, fmt.Errorf("syncing statistics commit marker directory: %w", err)
	}
	return layersLastWrite, imagesLastWrite, layersRecorded, imagesRecorded, nil
}

func reportFromManifest(manifest *statsSnapshotManifest) *LayerStatisticsReport {
	return &LayerStatisticsReport{
		Version:    statsSnapshotVersion,
		Generation: manifest.Generation,
		CreatedAt:  manifest.CreatedAt,
		Driver:     manifest.Driver,
		Changed:    true,
		Filesystem: manifest.Filesystem,
		Layers:     manifest.Stats,
	}
}

// discardCandidate removes a candidate directory and its payloads without
// touching the committed indexes.
func (m *statisticsManager) discardCandidate(manifest *statsSnapshotManifest) error {
	var errs []error
	remove := func(t *statsSnapshotTarget) {
		if t == nil || t.TempName == "" {
			return
		}
		path := filepath.Join(filepath.Dir(t.Path), t.TempName)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("removing candidate file %q: %w", path, err))
		}
	}
	remove(&manifest.Layers)
	remove(&manifest.Images)
	if err := os.RemoveAll(m.snapshotDir(manifest.Generation)); err != nil {
		errs = append(errs, fmt.Errorf("removing candidate snapshot directory for %q: %w", manifest.Generation, err))
	}
	removeEmptySnapshotsRoot(m.snapshotRoot())
	return errors.Join(errs...)
}

// removeEmptySnapshotsRoot removes the snapshots parent directory if no
// generation remains.  A failure (for example because another generation is
// being prepared concurrently) is ignored.
func removeEmptySnapshotsRoot(root string) {
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
		// Expected when the directory still contains another generation.
		return
	}
}

// runLocked executes a full refresh while the stores are locked.
func (m *statisticsManager) runLocked() (*LayerStatisticsReport, error) {
	fsStat, err := getFilesystemStatistics(m.store.graphRoot)
	if err != nil {
		return nil, fmt.Errorf("collecting filesystem statistics for %q: %w", m.store.graphRoot, err)
	}

	collected, err := m.collect()
	if err != nil {
		return nil, err
	}

	// A repeated refresh with identical results is a no-op: it neither
	// rewrites indexes nor advances lock files or the committed generation.
	// The same applies to a store without observable layers once a
	// generation has ever been committed.
	committed, committedErr := loadCommittedStatistics(m.layerDir)
	if committedErr != nil {
		return nil, committedErr
	}
	if committed != nil && (m.statisticsUnchanged(collected, committed.Generation) || len(collected) == 0) {
		committed.Changed = false
		return committed, nil
	}

	generation := stringid.GenerateRandomID()
	at := time.Now().UTC()
	layersPayload, imagesPayload, stats, err := m.buildCandidate(generation, at, collected)
	if err != nil {
		return nil, err
	}

	manifest, err := m.prepare(generation, at, fsStat, layersPayload, imagesPayload, stats)
	if err != nil {
		return nil, err
	}

	// Publication is idempotent (installed targets are skipped), so retry it
	// while we still hold the write locks instead of leaving the store in a
	// half-published state.  Persistent failures leave the candidate on disk
	// for startup recovery to complete; the in-memory indexes stay on the
	// previous generation until publication fully succeeds.
	var lwLayers, lwImages lockfile.LastWrite
	var layersRecorded, imagesRecorded bool
	const maxPublishAttempts = 3
	for attempt := 1; ; attempt++ {
		lwLayers, lwImages, layersRecorded, imagesRecorded, err = publishPrepared(manifest, m.rls.lockfile, m.img.lockfile)
		if err == nil {
			break
		}
		if attempt == maxPublishAttempts {
			return nil, fmt.Errorf("publishing statistics generation %q: %w", generation, err)
		}
		logrus.Warnf("Publishing statistics generation %q failed (attempt %d/%d), retrying: %v",
			generation, attempt, maxPublishAttempts, err)
	}
	// Only adopt the token of a lock this publication actually advanced;
	// resuming an already-installed target leaves its lock file untouched.
	if layersRecorded {
		m.rls.lastWrite = lwLayers
	}
	if imagesRecorded {
		m.img.lastWrite = lwImages
	}

	// Install the published generation in the live in-memory indexes.  The
	// candidate was built from these same records, so only the statistics
	// fields change; the layer maps keep pointing at the same pointers.
	for _, layer := range m.rls.layers {
		if layer.location != stableLayerLocation {
			continue
		}
		for _, c := range collected {
			if c.layer.ID == layer.ID {
				layer.StatsGeneration = generation
				layer.StatsAt = at
				layer.StatsSize = c.size
				layer.StatsInodes = c.inodes
				layer.StatsDiffSize = c.diffSize
				layer.StatsQuotaEnabled = c.quotaEnabled
				break
			}
		}
	}
	if info, statErr := os.Stat(m.layersPath); statErr == nil {
		m.rls.layerspathsModified[0] = info.ModTime()
	}
	for _, image := range m.img.images {
		image.StatsGeneration = generation
	}

	if err := os.RemoveAll(m.snapshotDir(generation)); err != nil {
		logrus.Errorf("Removing published statistics snapshot directory %q: %v", generation, err)
	}
	removeEmptySnapshotsRoot(m.snapshotRoot())
	// The payloads were renamed into place; remove leftovers defensively in
	// case publication found them already installed by an earlier attempt.
	_ = os.Remove(filepath.Join(m.layerDir, manifest.Layers.TempName))
	_ = os.Remove(filepath.Join(m.imageDir, manifest.Images.TempName))

	report := reportFromManifest(manifest)
	logrus.Debugf("Published statistics generation %q for %d layers (quota=%v)", generation, len(stats), m.quotaSupported)
	return report, nil
}

// RefreshLayerStatistics recomputes on-disk usage, diff sizes, quota usage
// and filesystem capacity for the layers of the store, verifies that the
// layer graph and the image index stay mutually consistent, and publishes
// the refreshed layer and image indexes as a single crash-safe generation.
//
// All statistics collection and validation happens in a candidate snapshot
// before any index file or lock file is modified.  If any collection,
// verification, write or sync step fails, the previously committed indexes
// remain fully readable and are not mixed with the candidate.  If the process
// terminates while publishing, the next store initialization either completes
// the unique candidate or discards it.
//
// A repeated call that observes identical statistics does not create a new
// generation.  Refreshing a read-only store fails with ErrStoreIsReadOnly,
// and drivers without filesystem quota simply report directory-walk usage.
func (s *store) RefreshLayerStatistics() (*LayerStatisticsReport, error) {
	if err := s.startUsingGraphDriver(); err != nil {
		return nil, err
	}
	defer s.stopUsingGraphDriver()

	rlsrw, err := s.getLayerStoreLocked()
	if err != nil {
		return nil, err
	}
	rls, ok := rlsrw.(*layerStore)
	if !ok {
		return nil, fmt.Errorf("unexpected layer store type %T", rlsrw)
	}
	roLayers, err := s.getROLayerStoresLocked()
	if err != nil {
		return nil, err
	}
	img, ok := s.imageStore.(*imageStore)
	if !ok {
		return nil, fmt.Errorf("unexpected image store type %T", s.imageStore)
	}

	if !rls.lockfile.IsReadWrite() || !img.lockfile.IsReadWrite() {
		return nil, fmt.Errorf("not allowed to refresh statistics on a read-only store: %w", ErrStoreIsReadOnly)
	}

	if err := rls.startWriting(); err != nil {
		return nil, err
	}
	defer rls.stopWriting()
	for _, ro := range roLayers {
		if err := ro.startReading(); err != nil {
			return nil, err
		}
		defer ro.stopReading()
	}
	if err := img.startWriting(); err != nil {
		return nil, err
	}
	defer img.stopWriting()

	manager := newStatisticsManager(s, rls, roLayers, img)
	return manager.runLocked()
}

// LayerStatistics returns the statistics of the last fully committed
// generation.  It never reads candidate snapshots, so the returned values are
// always consistent with the committed layer and image indexes.  It returns
// (nil, nil) if no statistics generation has ever been committed.
func (s *store) LayerStatistics() (*LayerStatisticsReport, error) {
	layerDir := filepath.Join(s.graphRoot, s.graphDriverName+"-layers")
	return loadCommittedStatistics(layerDir)
}

func loadCommittedStatistics(layerDir string) (*LayerStatisticsReport, error) {
	data, err := os.ReadFile(filepath.Join(layerDir, statsCommittedFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	report := &LayerStatisticsReport{}
	if err := json.Unmarshal(data, report); err != nil {
		return nil, fmt.Errorf("loading committed statistics marker from %q: %w", layerDir, err)
	}
	return report, nil
}

// validateSnapshot verifies that a prepared candidate is complete: the
// manifest is well-formed and every candidate payload exists with the exact
// recorded length and checksum.
func validateSnapshot(manifest *statsSnapshotManifest) error {
	if manifest.Version != statsSnapshotVersion {
		return fmt.Errorf("%w: unknown version %d", errStatisticsSnapshotInvalid, manifest.Version)
	}
	if manifest.State != statsSnapshotStateReady || manifest.Generation == "" {
		return fmt.Errorf("%w: invalid state or generation", errStatisticsSnapshotInvalid)
	}
	check := func(t *statsSnapshotTarget, expectedKind string) error {
		if t.Kind != expectedKind || t.Path == "" || t.LockPath == "" || t.TempName == "" || filepath.Base(t.TempName) != t.TempName {
			return fmt.Errorf("%w: malformed %s target", errStatisticsSnapshotInvalid, expectedKind)
		}
		if t.Size < 0 || t.SHA256 == "" {
			return fmt.Errorf("%w: missing %s payload digest", errStatisticsSnapshotInvalid, expectedKind)
		}
		tempPath := filepath.Join(filepath.Dir(t.Path), t.TempName)
		data, err := os.ReadFile(tempPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("%w: %s candidate payload unreadable: %w", errStatisticsSnapshotInvalid, expectedKind, err)
			}
			// The temp file is gone: this is valid only when the candidate
			// payload was already renamed into place by an interrupted
			// publication.
			state, stateErr := targetPublicationState(t)
			if stateErr != nil || state != "installed" {
				return fmt.Errorf("%w: %s candidate payload is missing and not installed", errStatisticsSnapshotInvalid, expectedKind)
			}
			return nil
		}
		if int64(len(data)) != t.Size || checksum(data) != t.SHA256 {
			return fmt.Errorf("%w: %s candidate payload checksum mismatch", errStatisticsSnapshotInvalid, expectedKind)
		}
		return nil
	}
	if err := check(&manifest.Layers, statsTargetLayers); err != nil {
		return err
	}
	return check(&manifest.Images, statsTargetImages)
}

// readSnapshotManifest loads and validates the single candidate described in
// a snapshot directory.
func readSnapshotManifest(dir string) (*statsSnapshotManifest, error) {
	manifest := &statsSnapshotManifest{}
	data, err := os.ReadFile(filepath.Join(dir, statsSnapshotManifestName))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errStatisticsSnapshotInvalid, err)
	}
	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, fmt.Errorf("%w: %v", errStatisticsSnapshotInvalid, err)
	}
	if err := validateSnapshot(manifest); err != nil {
		return nil, err
	}
	if filepath.Base(dir) != manifest.Generation {
		return nil, fmt.Errorf("%w: snapshot directory %q does not match generation %q",
			errStatisticsSnapshotInvalid, filepath.Base(dir), manifest.Generation)
	}
	return manifest, nil
}

// candidateState classifies a validated candidate relative to the on-disk
// indexes: "installed" (all targets already hold the candidate), "partial"
// (a mix of candidate and recorded base), "base" (nothing installed yet), or
// "superseded" (at least one target holds unrelated newer content).
func candidateState(manifest *statsSnapshotManifest) (string, error) {
	states := make([]string, 0, 2)
	for _, t := range []*statsSnapshotTarget{&manifest.Layers, &manifest.Images} {
		state, err := targetPublicationState(t)
		if err != nil {
			return "", err
		}
		states = append(states, state)
	}
	installed, base := 0, 0
	for _, state := range states {
		switch state {
		case "installed":
			installed++
		case "base":
			base++
		case "other":
			return "superseded", nil
		}
	}
	switch {
	case installed == len(states):
		return "installed", nil
	case installed > 0:
		return "partial", nil
	default:
		return "base", nil
	}
}

// recoverStatisticsSnapshots resolves candidate statistics generations left
// behind by a process that died while preparing or publishing one.  It runs
// during store initialization before any layer or image store serves
// requests, holding the same index write locks as a live refresh so that the
// two can never interleave.
//
// Recovery is deterministic:
//   - a unique valid candidate whose targets are all still base/candidate
//     content is completed ("unique publish");
//   - a candidate that is already partially installed is completed as well;
//   - corrupt candidates, and candidates whose indexes were taken over by a
//     newer unrelated write, are discarded without touching the indexes;
//   - if several viable candidates exist and none is clearly the in-flight
//     one, all of them are discarded, since no unique publish can be chosen.
func recoverStatisticsSnapshots(layerDir, imageDir string) error {
	// Locks are taken in the same order as by the live refresh path.
	layersLock, err := lockfile.GetLockFile(filepath.Join(layerDir, "layers.lock"))
	if err != nil {
		return fmt.Errorf("opening layer index lock for statistics recovery: %w", err)
	}
	layersLock.Lock()
	defer layersLock.Unlock()

	imagesLock, err := lockfile.GetLockFile(filepath.Join(imageDir, "images.lock"))
	if err != nil {
		return fmt.Errorf("opening image index lock for statistics recovery: %w", err)
	}
	imagesLock.Lock()
	defer imagesLock.Unlock()

	return recoverStatisticsSnapshotsLocked(layerDir, imageDir, layersLock, imagesLock)
}

// recoverStatisticsSnapshotsLocked is the body of recoverStatisticsSnapshots
// with the index write locks already held.
func recoverStatisticsSnapshotsLocked(layerDir, imageDir string, layersLock, imagesLock statisticsWriteLock) error {
	root := filepath.Join(layerDir, statsSnapshotDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return sweepOrphanStatisticsTemps(layerDir, imageDir)
		}
		return err
	}

	type found struct {
		manifest *statsSnapshotManifest
		invalid  bool
		dir      string
	}
	candidates := make([]found, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		manifest, readErr := readSnapshotManifest(dir)
		candidates = append(candidates, found{manifest: manifest, invalid: readErr != nil, dir: dir})
		if readErr != nil {
			logrus.Warnf("Discarding invalid statistics candidate in %q: %v", dir, readErr)
		}
	}

	discard := func(f found) {
		if f.manifest != nil {
			for _, t := range []*statsSnapshotTarget{&f.manifest.Layers, &f.manifest.Images} {
				if err := os.Remove(filepath.Join(filepath.Dir(t.Path), t.TempName)); err != nil && !os.IsNotExist(err) {
					logrus.Warnf("Removing discarded statistics candidate payload %q: %v", t.TempName, err)
				}
			}
		}
		if err := os.RemoveAll(f.dir); err != nil {
			logrus.Warnf("Removing discarded statistics candidate directory %q: %v", f.dir, err)
		}
		removeEmptySnapshotsRoot(root)
	}

	valid := make([]found, 0, len(candidates))
	for _, f := range candidates {
		if f.invalid {
			discard(f)
			continue
		}
		state, stateErr := candidateState(f.manifest)
		if stateErr != nil {
			return stateErr
		}
		if state == "superseded" {
			logrus.Warnf("Discarding statistics candidate %q: indexes were modified by a newer write", f.manifest.Generation)
			discard(f)
			continue
		}
		valid = append(valid, f)
	}

	// At most one candidate can be the in-flight one: the unique candidate
	// that has already started installing.  If publication never started and
	// more than one candidate exists, no unique publish is possible, so all
	// of them are discarded.
	var toPublish *found
	switch {
	case len(valid) == 1:
		toPublish = &valid[0]
	case len(valid) > 1:
		for i := range valid {
			state, _ := candidateState(valid[i].manifest)
			if state == "partial" || state == "installed" {
				if toPublish == nil {
					chosen := valid[i]
					toPublish = &chosen
				} else {
					toPublish = nil
					break
				}
			}
		}
		for i := range valid {
			if toPublish == nil || valid[i].manifest.Generation != toPublish.manifest.Generation {
				discard(valid[i])
			}
		}
	}

	if toPublish != nil {
		if err := finishRecoveryPublication(toPublish.manifest, layersLock, imagesLock); err != nil {
			return err
		}
		if err := os.RemoveAll(toPublish.dir); err != nil {
			logrus.Warnf("Removing completed statistics candidate directory %q: %v", toPublish.dir, err)
		}
		removeEmptySnapshotsRoot(root)
	}

	if err := sweepOrphanStatisticsTemps(layerDir, imageDir); err != nil {
		return err
	}
	removeEmptySnapshotsRoot(root)
	return nil
}

// finishRecoveryPublication resumes publication of the unique candidate while
// the caller holds the index write locks.  Re-classifying under the locks
// ensures that the candidate cannot be overtaken while it is completed.
func finishRecoveryPublication(manifest *statsSnapshotManifest, layersLock, imagesLock statisticsWriteLock) error {
	state, err := candidateState(manifest)
	if err != nil {
		return err
	}
	switch state {
	case "installed", "partial", "base":
		// Resumable.
	case "superseded":
		return errStatisticsSnapshotSuperseded
	}

	if _, _, _, _, err := publishPrepared(manifest, layersLock, imagesLock); err != nil {
		return fmt.Errorf("completing statistics generation %q during startup recovery: %w", manifest.Generation, err)
	}
	logrus.Infof("Completed statistics generation %q during startup recovery", manifest.Generation)
	return nil
}

// sweepOrphanStatisticsTemps removes candidate payload files that are not
// referenced by any remaining manifest.  They can only be leftovers of a
// process that died before (or while) writing the manifest, and can never be
// completed.  It must run after all still-described candidates have been
// either published or discarded.
func sweepOrphanStatisticsTemps(layerDir, imageDir string) error {
	var errs []error
	for _, dir := range []string{layerDir, imageDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if filepath.Base(name) != name || len(name) <= len(statsCandidateTempPrefix) {
				continue
			}
			if name[:len(statsCandidateTempPrefix)] != statsCandidateTempPrefix {
				continue
			}
			path := filepath.Join(dir, name)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("removing orphan statistics candidate file %q: %w", path, err))
			}
		}
	}
	return errors.Join(errs...)
}
