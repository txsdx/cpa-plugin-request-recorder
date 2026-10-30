package recorder

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

func (s *Store) view(fn func(*bolt.Tx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.View(fn)
}
func (s *Store) update(cost int, fn func(*bolt.Tx) error) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	return s.updateLocked(cost, fn)
}
func (s *Store) updateLocked(cost int, fn func(*bolt.Tx) error) error {
	s.mu.RLock()
	err := s.db.Update(fn)
	s.mu.RUnlock()
	if err == nil && s.journal != nil && !s.journal.overflow {
		if cost < 4096 {
			cost = 4096
		}
		if s.journal.bytes+cost*2 > 32<<20 || len(s.journal.operations) >= 2048 {
			s.journal.overflow = true
			s.journal.operations = nil
		} else {
			s.journal.bytes += cost * 2
			s.journal.operations = append(s.journal.operations, fn)
		}
	}
	return err
}

type databaseSize struct {
	File, Used, Free int64
	Records          int
	Evicted          uint64
}

func (s *Store) databaseSize() databaseSize {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var size databaseSize
	if info, err := os.Stat(s.db.Path()); err == nil {
		size.File = info.Size()
	}
	stats := s.db.Stats()
	size.Free = int64(stats.FreeAlloc)
	_ = s.db.View(func(tx *bolt.Tx) error {
		size.Used = tx.Size() - size.Free
		size.Records = int(readCatalogCounts(tx).total)
		if value := tx.Bucket(storageMeta).Get([]byte("capacity_evicted")); len(value) == 8 {
			size.Evicted = binary.BigEndian.Uint64(value)
		}
		return nil
	})
	return size
}
func (s *Store) storageQuick() map[string]any {
	size := s.databaseSize()
	s.maintenance.Lock()
	running, lastError := s.running, s.lastMaintenanceError
	s.maintenance.Unlock()
	return map[string]any{"database_file_bytes": size.File, "database_free_page_bytes": size.Free, "database_storage_format": "conversation-v1/chunks-v1", "max_database_bytes": s.limit, "capacity_evicted_records": size.Evicted, "database_maintenance_running": running, "database_maintenance_error": lastError}
}
func (s *Store) StorageStats() (map[string]any, error) {
	result := s.storageQuick()
	var recordsCount, chunksCount int
	var metadata, logical, unique, stored int64
	err := s.view(func(tx *bolt.Tx) error {
		if err := tx.Bucket(records).ForEach(func(_, value []byte) error {
			var row struct {
				Bodies map[string]bodyManifest `json:"_bodies"`
			}
			if err := json.Unmarshal(value, &row); err != nil {
				return err
			}
			recordsCount++
			metadata += int64(len(value))
			for _, body := range row.Bodies {
				logical += int64(body.Size)
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(bodyChunks).ForEach(func(_, value []byte) error {
			if len(value) < 5 {
				return errors.New("truncated body chunk")
			}
			chunksCount++
			unique += int64(binary.BigEndian.Uint32(value[1:5]))
			stored += int64(len(value))
			return nil
		})
	})
	result["records"], result["record_metadata_bytes"] = recordsCount, metadata
	result["body_logical_bytes"], result["body_unique_bytes"] = logical, unique
	result["body_stored_bytes"], result["body_chunks"] = stored, chunksCount
	return result, err
}

func (s *Store) requestMaintenance() {
	size := s.databaseSize()
	if size.Used <= s.limit*55/100 && size.File <= s.limit && (size.File < 8<<20 || size.Free < size.File/3) {
		return
	}
	s.maintenance.Lock()
	if s.closing || s.running {
		s.maintenance.Unlock()
		return
	}
	s.running = true
	s.wg.Add(1)
	s.maintenance.Unlock()
	go func() {
		defer s.wg.Done()
		err := s.enforceCapacity()
		s.maintenance.Lock()
		s.running = false
		s.lastMaintenanceError = ""
		if err != nil {
			s.lastMaintenanceError = err.Error()
		}
		s.maintenance.Unlock()
	}()
}
func (s *Store) enforceCapacity() error {
	// Leave room for B+tree fill, copy-on-write and the next request. The public
	// limit measures the active file, not just compressed payload lengths.
	for {
		size := s.databaseSize()
		if size.Used <= s.limit*45/100 || size.Records == 0 {
			break
		}
		batch := size.Records/20 + 1
		if batch > 128 {
			batch = 128
		}
		if err := s.update(4096, func(tx *bolt.Tx) error {
			var removed uint64
			for i := 0; i < batch; i++ {
				_, id := tx.Bucket(ordering).Cursor().First()
				if id == nil {
					break
				}
				id = append([]byte(nil), id...)
				var summary Summary
				if err := json.Unmarshal(tx.Bucket(summaries).Get(id), &summary); err != nil {
					return err
				}
				if err := removeStoredRecord(tx, id, summary); err != nil {
					return err
				}
				removed++
			}
			meta := tx.Bucket(storageMeta)
			var total uint64
			if value := meta.Get([]byte("capacity_evicted")); len(value) == 8 {
				total = binary.BigEndian.Uint64(value)
			}
			var value [8]byte
			binary.BigEndian.PutUint64(value[:], total+removed)
			return meta.Put([]byte("capacity_evicted"), value[:])
		}); err != nil {
			return err
		}
	}
	size := s.databaseSize()
	if size.Records == 0 && size.Used > s.limit {
		return errors.New("备注或索引已超过数据库容量，请提高 max_database_bytes")
	}
	if size.File > s.limit || size.File >= 8<<20 && size.Free > size.File/3 {
		return s.compactOnline()
	}
	return nil
}
func removeStoredRecord(tx *bolt.Tx, id []byte, summary Summary) error {
	if err := updateCatalogCounts(tx, &summary, nil); err != nil {
		return err
	}
	if err := clearConversationKeys(tx, id); err != nil {
		return err
	}
	if summary.RequestID != "" && string(tx.Bucket(requestRecords).Get([]byte(summary.RequestID))) == string(id) {
		if err := tx.Bucket(requestRecords).Delete([]byte(summary.RequestID)); err != nil {
			return err
		}
	}
	ids, err := storedChunkIDs(tx.Bucket(records).Get(id))
	if err != nil {
		return err
	}
	for id := range ids {
		if err := releaseBodyChunk(tx, id); err != nil {
			return err
		}
	}
	for _, name := range [][]byte{records, summaries, imageAssets, imageJobs} {
		if err := tx.Bucket(name).Delete(id); err != nil {
			return err
		}
	}
	if err := tx.Bucket(traces).Delete([]byte(summary.TraceID + "\x00" + summary.ID)); err != nil {
		return err
	}
	return tx.Bucket(ordering).Delete(orderKey(summary))
}

// Copy a consistent snapshot in bounded transactions while normal writes
// continue. Replay the bounded journal under a short writer barrier, then swap.
// Overflow cancels the copy rather than retaining unbounded request bodies.
func (s *Store) compactOnline() error {
	s.writes.Lock()
	s.mu.RLock()
	snapshot, err := s.db.Begin(false)
	path := s.db.Path()
	s.mu.RUnlock()
	if err != nil {
		s.writes.Unlock()
		return err
	}
	journal := &mutationJournal{}
	s.journal = journal
	s.writes.Unlock()
	f, err := os.CreateTemp(filepath.Dir(path), ".recorder-compact-*")
	if err != nil {
		snapshot.Rollback()
		s.writes.Lock()
		s.journal = nil
		s.writes.Unlock()
		return err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	dest, err := openBolt(tmp)
	if err == nil {
		err = copySnapshot(snapshot, dest)
	}
	snapshot.Rollback() // release mmap before waiting for a writer that may grow it
	s.writes.Lock()
	defer s.writes.Unlock()
	s.journal = nil
	if err != nil {
		if dest != nil {
			dest.Close()
		}
		return err
	}
	defer dest.Close()
	if journal.overflow {
		return errors.New("整理期间写入较多，已取消本次整理；下轮自动重试")
	}
	if err := dest.Update(func(tx *bolt.Tx) error {
		for _, operation := range journal.operations {
			if err := operation(tx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := dest.View(func(tx *bolt.Tx) error {
		var first error
		for err := range tx.Check() {
			if first == nil {
				first = err
			}
		}
		return first
	}); err != nil {
		return err
	}
	if err := dest.Close(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := tmp + ".rollback"
	if err := os.Link(path, backup); err != nil {
		if err := copyDatabaseFile(path, backup); err != nil {
			return err
		}
	}
	if err := s.db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		if original, openErr := openBolt(path); openErr == nil {
			s.db = original
			_ = os.Remove(backup)
		}
		return err
	}
	reopened, err := openBolt(path)
	if err != nil {
		if restoreErr := os.Rename(backup, path); restoreErr != nil {
			return fmt.Errorf("整理失败，原库保留在 %s: %w", backup, restoreErr)
		}
		if original, openErr := openBolt(path); openErr == nil {
			s.db = original
		}
		return err
	}
	s.db = reopened
	_ = os.Remove(backup)
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}
func openBolt(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err == nil {
		db.AllocSize = 64 << 10
	}
	return db, err
}
func copySnapshot(source *bolt.Tx, dest *bolt.DB) (err error) {
	tx, err := dest.Begin(true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var written int
	err = source.ForEach(func(name []byte, bucket *bolt.Bucket) error {
		b, err := tx.CreateBucket(name)
		if err != nil {
			return err
		}
		if err := b.SetSequence(bucket.Sequence()); err != nil {
			return err
		}
		return bucket.ForEach(func(key, value []byte) error {
			if value == nil {
				return errors.New("unexpected nested recorder bucket")
			}
			if written >= 4<<20 {
				if err := tx.Commit(); err != nil {
					return err
				}
				var err error
				tx, err = dest.Begin(true)
				if err != nil {
					return err
				}
				written = 0
			}
			b := tx.Bucket(name)
			b.FillPercent = 0.9
			written += len(key) + len(value)
			return b.Put(key, value)
		})
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}
func copyDatabaseFile(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(dest)
		return err
	}
	return closeErr
}
