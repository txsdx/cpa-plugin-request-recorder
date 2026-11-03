package recorder

import (
	"encoding/json"
	bolt "go.etcd.io/bbolt"
)

type archiveStore struct {
	*Store
	assets, jobs []byte
}

func (s *Store) assetStore(files bool) archiveStore {
	if files {
		return archiveStore{s, fileAssets, fileJobs}
	}
	return archiveStore{s, imageAssets, imageJobs}
}

// Image metadata is separate from large request bodies. Download progress updates
// cannot overwrite concurrent usage updates or rewrite multi-MiB bodies.
func (s archiveStore) get(id string) ([]ArchiveAsset, error) {
	out := []ArchiveAsset{}
	err := s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		if raw := tx.Bucket(s.assets).Get([]byte(id)); raw != nil {
			return json.Unmarshal(raw, &out)
		}
		return nil
	})
	return out, err
}
func (s archiveStore) set(id string, assets []ArchiveAsset) error {
	raw, err := json.Marshal(assets)
	if err != nil {
		return err
	}
	return s.update(len(raw), func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return tx.Bucket(s.assets).Put([]byte(id), raw)
	})
}
func (s archiveStore) references() (map[string]bool, error) {
	refs := map[string]bool{}
	err := s.view(func(tx *bolt.Tx) error {
		return tx.Bucket(s.assets).ForEach(func(_, raw []byte) error {
			var images []ArchiveAsset
			if err := json.Unmarshal(raw, &images); err != nil {
				return err
			}
			for _, im := range images {
				if im.Status == "ready" {
					refs[im.Hash] = true
				}
			}
			return nil
		})
	})
	return refs, err
}
func (s archiveStore) pending(limit int) ([]string, error) {
	ids := []string{}
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(s.jobs).Cursor()
		for k, _ := c.First(); k != nil && len(ids) < limit; k, _ = c.Next() {
			ids = append(ids, string(k))
		}
		return nil
	})
	return ids, err
}
func (s archiveStore) queue(id string) error {
	queued := false
	if err := s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		queued = tx.Bucket(s.jobs).Get([]byte(id)) != nil
		return nil
	}); err != nil {
		return err
	}
	if queued {
		return nil
	}
	return s.update(0, func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return tx.Bucket(s.jobs).Put([]byte(id), []byte{1})
	})
}
func (s archiveStore) finish(id string) error {
	return s.update(0, func(tx *bolt.Tx) error {
		return tx.Bucket(s.jobs).Delete([]byte(id))
	})
}

func (s archiveStore) failed(recordID, id, message string) error {
	return s.update(0, func(tx *bolt.Tx) error {
		b := tx.Bucket(s.assets)
		raw := b.Get([]byte(recordID))
		if raw == nil {
			return ErrNotFound
		}
		var assets []ArchiveAsset
		if err := json.Unmarshal(raw, &assets); err != nil {
			return err
		}
		for i := range assets {
			if assets[i].ID == id {
				assets[i].Status = "failed"
				assets[i].Error = message
			}
		}
		encoded, err := json.Marshal(assets)
		if err != nil {
			return err
		}
		return b.Put([]byte(recordID), encoded)
	})
}

func (s *Store) GetImages(id string) ([]ArchiveAsset, error) { return s.assetStore(false).get(id) }
func (s *Store) SetImages(id string, a []ArchiveAsset) error { return s.assetStore(false).set(id, a) }
func (s *Store) ImageReferences() (map[string]bool, error)   { return s.assetStore(false).references() }
func (s *Store) PendingImageRecords(n int) ([]string, error) { return s.assetStore(false).pending(n) }
func (s *Store) QueueImages(id string) error                 { return s.assetStore(false).queue(id) }
func (s *Store) FinishImages(id string) error                { return s.assetStore(false).finish(id) }
func (s *Store) ImageFailed(record, id, message string) error {
	return s.assetStore(false).failed(record, id, message)
}
func (s *Store) GetFiles(id string) ([]ArchiveAsset, error) { return s.assetStore(true).get(id) }
