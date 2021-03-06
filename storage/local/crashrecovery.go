// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package local

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync/atomic"

	"github.com/golang/glog"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/storage/local/codable"
	"github.com/prometheus/prometheus/storage/local/index"
)

// recoverFromCrash is called by loadSeriesMapAndHeads if the persistence
// appears to be dirty after the loading (either because the loading resulted in
// an error or because the persistence was dirty from the start). Not goroutine
// safe. Only call before anything else is running (except index processing
// queue as started by newPersistence).
func (p *persistence) recoverFromCrash(fingerprintToSeries map[clientmodel.Fingerprint]*memorySeries) error {
	// TODO(beorn): We need proper tests for the crash recovery.
	glog.Warning("Starting crash recovery. Prometheus is inoperational until complete.")

	fpsSeen := map[clientmodel.Fingerprint]struct{}{}
	count := 0
	seriesDirNameFmt := fmt.Sprintf("%%0%dx", seriesDirNameLen)

	glog.Info("Scanning files.")
	for i := 0; i < 1<<(seriesDirNameLen*4); i++ {
		dirname := path.Join(p.basePath, fmt.Sprintf(seriesDirNameFmt, i))
		dir, err := os.Open(dirname)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		defer dir.Close()
		for fis := []os.FileInfo{}; err != io.EOF; fis, err = dir.Readdir(1024) {
			if err != nil {
				return err
			}
			for _, fi := range fis {
				fp, ok := p.sanitizeSeries(dirname, fi, fingerprintToSeries)
				if ok {
					fpsSeen[fp] = struct{}{}
				}
				count++
				if count%10000 == 0 {
					glog.Infof("%d files scanned.", count)
				}
			}
		}
	}
	glog.Infof("File scan complete. %d series found.", len(fpsSeen))

	glog.Info("Checking for series without series file.")
	for fp, s := range fingerprintToSeries {
		if _, seen := fpsSeen[fp]; !seen {
			// fp exists in fingerprintToSeries, but has no representation on disk.
			if s.headChunkClosed {
				// Oops, everything including the head chunk was
				// already persisted, but nothing on disk.
				// Thus, we lost that series completely. Clean
				// up the remnants.
				delete(fingerprintToSeries, fp)
				if err := p.purgeArchivedMetric(fp); err != nil {
					// Purging the archived metric didn't work, so try
					// to unindex it, just in case it's in the indexes.
					p.unindexMetric(fp, s.metric)
				}
				glog.Warningf("Lost series detected: fingerprint %v, metric %v.", fp, s.metric)
				continue
			}
			// If we are here, the only chunks we have are the chunks in the checkpoint.
			// Adjust things accordingly.
			if s.persistWatermark > 0 || s.chunkDescsOffset != 0 {
				minLostChunks := s.persistWatermark + s.chunkDescsOffset
				if minLostChunks <= 0 {
					glog.Warningf(
						"Possible loss of chunks for fingerprint %v, metric %v.",
						fp, s.metric,
					)
				} else {
					glog.Warningf(
						"Lost at least %d chunks for fingerprint %v, metric %v.",
						minLostChunks, fp, s.metric,
					)
				}
				s.chunkDescs = append(
					make([]*chunkDesc, 0, len(s.chunkDescs)-s.persistWatermark),
					s.chunkDescs[s.persistWatermark:]...,
				)
				numMemChunkDescs.Sub(float64(s.persistWatermark))
				s.persistWatermark = 0
				s.chunkDescsOffset = 0
			}
			fpsSeen[fp] = struct{}{} // Add so that fpsSeen is complete.
		}
	}
	glog.Info("Check for series without series file complete.")

	if err := p.cleanUpArchiveIndexes(fingerprintToSeries, fpsSeen); err != nil {
		return err
	}
	if err := p.rebuildLabelIndexes(fingerprintToSeries); err != nil {
		return err
	}

	p.setDirty(false)
	glog.Warning("Crash recovery complete.")
	return nil
}

// sanitizeSeries sanitizes a series based on its series file as defined by the
// provided directory and FileInfo.  The method returns the fingerprint as
// derived from the directory and file name, and whether the provided file has
// been sanitized. A file that failed to be sanitized is moved into the
// "orphaned" sub-directory, if possible.
//
// The following steps are performed:
//
// - A file whose name doesn't comply with the naming scheme of a series file is
//   simply moved into the orphaned directory.
//
// - If the size of the series file isn't a multiple of the chunk size,
//   extraneous bytes are truncated.  If the truncation fails, the file is
//   moved into the orphaned directory.
//
// - A file that is empty (after truncation) is deleted.
//
// - A series that is not archived (i.e. it is in the fingerprintToSeries map)
//   is checked for consistency of its various parameters (like persist
//   watermark, offset of chunkDescs etc.). In particular, overlap between an
//   in-memory head chunk with the most recent persisted chunk is
//   checked. Inconsistencies are rectified.
//
// - A series that is archived (i.e. it is not in the fingerprintToSeries map)
//   is checked for its presence in the index of archived series. If it cannot
//   be found there, it is moved into the orphaned directory.
func (p *persistence) sanitizeSeries(
	dirname string, fi os.FileInfo, fingerprintToSeries map[clientmodel.Fingerprint]*memorySeries,
) (clientmodel.Fingerprint, bool) {
	filename := path.Join(dirname, fi.Name())
	purge := func() {
		var err error
		defer func() {
			if err != nil {
				glog.Errorf("Failed to move lost series file %s to orphaned directory, deleting it instead. Error was: %s", filename, err)
				if err = os.Remove(filename); err != nil {
					glog.Errorf("Even deleting file %s did not work: %s", filename, err)
				}
			}
		}()
		orphanedDir := path.Join(p.basePath, "orphaned", path.Base(dirname))
		if err = os.MkdirAll(orphanedDir, 0700); err != nil {
			return
		}
		if err = os.Rename(filename, path.Join(orphanedDir, fi.Name())); err != nil {
			return
		}
	}

	var fp clientmodel.Fingerprint
	if len(fi.Name()) != fpLen-seriesDirNameLen+len(seriesFileSuffix) ||
		!strings.HasSuffix(fi.Name(), seriesFileSuffix) {
		glog.Warningf("Unexpected series file name %s.", filename)
		purge()
		return fp, false
	}
	if err := fp.LoadFromString(path.Base(dirname) + fi.Name()[:fpLen-seriesDirNameLen]); err != nil {
		glog.Warningf("Error parsing file name %s: %s", filename, err)
		purge()
		return fp, false
	}

	bytesToTrim := fi.Size() % int64(chunkLenWithHeader)
	chunksInFile := int(fi.Size()) / chunkLenWithHeader
	modTime := fi.ModTime()
	if bytesToTrim != 0 {
		glog.Warningf(
			"Truncating file %s to exactly %d chunks, trimming %d extraneous bytes.",
			filename, chunksInFile, bytesToTrim,
		)
		f, err := os.OpenFile(filename, os.O_WRONLY, 0640)
		if err != nil {
			glog.Errorf("Could not open file %s: %s", filename, err)
			purge()
			return fp, false
		}
		if err := f.Truncate(fi.Size() - bytesToTrim); err != nil {
			glog.Errorf("Failed to truncate file %s: %s", filename, err)
			purge()
			return fp, false
		}
	}
	if chunksInFile == 0 {
		glog.Warningf("No chunks left in file %s.", filename)
		purge()
		return fp, false
	}

	s, ok := fingerprintToSeries[fp]
	if ok { // This series is supposed to not be archived.
		if s == nil {
			panic("fingerprint mapped to nil pointer")
		}
		if !p.pedanticChecks &&
			bytesToTrim == 0 &&
			s.chunkDescsOffset != -1 &&
			chunksInFile == s.chunkDescsOffset+s.persistWatermark &&
			modTime.Equal(s.modTime) {
			// Everything is consistent. We are good.
			return fp, true
		}
		// If we are here, we cannot be sure the series file is
		// consistent with the checkpoint, so we have to take a closer
		// look.
		if s.headChunkClosed {
			// This is the easy case as we don't have any chunks in
			// heads.db. Treat this series as a freshly unarchived
			// one. No chunks or chunkDescs in memory, no current
			// head chunk.
			glog.Warningf(
				"Treating recovered metric %v, fingerprint %v, as freshly unarchived, with %d chunks in series file.",
				s.metric, fp, chunksInFile,
			)
			s.chunkDescs = nil
			s.chunkDescsOffset = chunksInFile
			s.persistWatermark = 0
			s.modTime = modTime
			return fp, true
		}
		// This is the tricky one: We have chunks from heads.db, but
		// some of those chunks might already be in the series
		// file. Strategy: Take the last time of the most recent chunk
		// in the series file. Then find the oldest chunk among those
		// from heads.db that has a first time later or equal to the
		// last time from the series file. Throw away the older chunks
		// from heads.db and stitch the parts together.

		// First, throw away the chunkDescs without chunks.
		s.chunkDescs = s.chunkDescs[s.persistWatermark:]
		numMemChunkDescs.Sub(float64(s.persistWatermark))
		// Load all the chunk descs (which assumes we have none from the future).
		cds, err := p.loadChunkDescs(fp, clientmodel.Now())
		if err != nil {
			glog.Errorf(
				"Failed to load chunk descriptors for metric %v, fingerprint %v: %s",
				s.metric, fp, err,
			)
			purge()
			return fp, false
		}
		s.persistWatermark = len(cds)
		s.chunkDescsOffset = 0
		s.modTime = modTime

		lastTime := cds[len(cds)-1].lastTime()
		keepIdx := -1
		for i, cd := range s.chunkDescs {
			if cd.firstTime() >= lastTime {
				keepIdx = i
				break
			}
		}
		if keepIdx == -1 {
			glog.Warningf(
				"Recovered metric %v, fingerprint %v: all %d chunks recovered from series file.",
				s.metric, fp, chunksInFile,
			)
			numMemChunkDescs.Sub(float64(len(s.chunkDescs)))
			atomic.AddInt64(&numMemChunks, int64(-len(s.chunkDescs)))
			s.chunkDescs = cds
			s.headChunkClosed = true
			return fp, true
		}
		glog.Warningf(
			"Recovered metric %v, fingerprint %v: recovered %d chunks from series file, recovered %d chunks from checkpoint.",
			s.metric, fp, chunksInFile, len(s.chunkDescs)-keepIdx,
		)
		numMemChunkDescs.Sub(float64(keepIdx))
		atomic.AddInt64(&numMemChunks, int64(-keepIdx))
		s.chunkDescs = append(cds, s.chunkDescs[keepIdx:]...)
		return fp, true
	}
	// This series is supposed to be archived.
	metric, err := p.getArchivedMetric(fp)
	if err != nil {
		glog.Errorf(
			"Fingerprint %v assumed archived but couldn't be looked up in archived index: %s",
			fp, err,
		)
		purge()
		return fp, false
	}
	if metric == nil {
		glog.Warningf(
			"Fingerprint %v assumed archived but couldn't be found in archived index.",
			fp,
		)
		purge()
		return fp, false
	}
	// This series looks like a properly archived one.
	return fp, true
}

func (p *persistence) cleanUpArchiveIndexes(
	fpToSeries map[clientmodel.Fingerprint]*memorySeries,
	fpsSeen map[clientmodel.Fingerprint]struct{},
) error {
	glog.Info("Cleaning up archive indexes.")
	var fp codable.Fingerprint
	var m codable.Metric
	count := 0
	if err := p.archivedFingerprintToMetrics.ForEach(func(kv index.KeyValueAccessor) error {
		count++
		if count%10000 == 0 {
			glog.Infof("%d archived metrics checked.", count)
		}
		if err := kv.Key(&fp); err != nil {
			return err
		}
		_, fpSeen := fpsSeen[clientmodel.Fingerprint(fp)]
		inMemory := false
		if fpSeen {
			_, inMemory = fpToSeries[clientmodel.Fingerprint(fp)]
		}
		if !fpSeen || inMemory {
			if inMemory {
				glog.Warningf("Archive clean-up: Fingerprint %v is not archived. Purging from archive indexes.", clientmodel.Fingerprint(fp))
			}
			if !fpSeen {
				glog.Warningf("Archive clean-up: Fingerprint %v is unknown. Purging from archive indexes.", clientmodel.Fingerprint(fp))
			}
			// It's fine if the fp is not in the archive indexes.
			if _, err := p.archivedFingerprintToMetrics.Delete(fp); err != nil {
				return err
			}
			// Delete from timerange index, too.
			_, err := p.archivedFingerprintToTimeRange.Delete(fp)
			return err
		}
		// fp is legitimately archived. Make sure it is in timerange index, too.
		has, err := p.archivedFingerprintToTimeRange.Has(fp)
		if err != nil {
			return err
		}
		if has {
			return nil // All good.
		}
		glog.Warningf("Archive clean-up: Fingerprint %v is not in time-range index. Unarchiving it for recovery.")
		// Again, it's fine if fp is not in the archive index.
		if _, err := p.archivedFingerprintToMetrics.Delete(fp); err != nil {
			return err
		}
		if err := kv.Value(&m); err != nil {
			return err
		}
		series := newMemorySeries(clientmodel.Metric(m), false, clientmodel.Earliest)
		cds, err := p.loadChunkDescs(clientmodel.Fingerprint(fp), clientmodel.Now())
		if err != nil {
			return err
		}
		series.chunkDescs = cds
		series.chunkDescsOffset = 0
		series.persistWatermark = len(cds)
		fpToSeries[clientmodel.Fingerprint(fp)] = series
		return nil
	}); err != nil {
		return err
	}
	count = 0
	if err := p.archivedFingerprintToTimeRange.ForEach(func(kv index.KeyValueAccessor) error {
		count++
		if count%10000 == 0 {
			glog.Infof("%d archived time ranges checked.", count)
		}
		if err := kv.Key(&fp); err != nil {
			return err
		}
		has, err := p.archivedFingerprintToMetrics.Has(fp)
		if err != nil {
			return err
		}
		if has {
			return nil // All good.
		}
		glog.Warningf("Archive clean-up: Purging unknown fingerprint %v in time-range index.", fp)
		deleted, err := p.archivedFingerprintToTimeRange.Delete(fp)
		if err != nil {
			return err
		}
		if !deleted {
			glog.Errorf("Fingerprint %v to be deleted from archivedFingerprintToTimeRange not found. This should never happen.", fp)
		}
		return nil
	}); err != nil {
		return err
	}
	glog.Info("Clean-up of archive indexes complete.")
	return nil
}

func (p *persistence) rebuildLabelIndexes(
	fpToSeries map[clientmodel.Fingerprint]*memorySeries,
) error {
	count := 0
	glog.Info("Rebuilding label indexes.")
	glog.Info("Indexing metrics in memory.")
	for fp, s := range fpToSeries {
		p.indexMetric(fp, s.metric)
		count++
		if count%10000 == 0 {
			glog.Infof("%d metrics queued for indexing.", count)
		}
	}
	glog.Info("Indexing archived metrics.")
	var fp codable.Fingerprint
	var m codable.Metric
	if err := p.archivedFingerprintToMetrics.ForEach(func(kv index.KeyValueAccessor) error {
		if err := kv.Key(&fp); err != nil {
			return err
		}
		if err := kv.Value(&m); err != nil {
			return err
		}
		p.indexMetric(clientmodel.Fingerprint(fp), clientmodel.Metric(m))
		count++
		if count%10000 == 0 {
			glog.Infof("%d metrics queued for indexing.", count)
		}
		return nil
	}); err != nil {
		return err
	}
	glog.Info("All requests for rebuilding the label indexes queued. (Actual processing may lag behind.)")
	return nil
}
