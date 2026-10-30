package recorder

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"

	bolt "go.etcd.io/bbolt"
)

var catalogCountsKey = []byte("catalog_counts")

// Counts and the summary bucket's sequence belong to the same transaction as
// the rows. A snapshot never combines a new count with an older page.
type catalogCounts struct{ total, rejected, negativeDuration uint64 }

func readCatalogCounts(tx *bolt.Tx) catalogCounts {
	raw := tx.Bucket(storageMeta).Get(catalogCountsKey)
	if len(raw) != 24 {
		return catalogCounts{}
	}
	return catalogCounts{binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:16]), binary.BigEndian.Uint64(raw[16:])}
}

func writeCatalogCounts(tx *bolt.Tx, counts catalogCounts) error {
	var raw [24]byte
	binary.BigEndian.PutUint64(raw[:8], counts.total)
	binary.BigEndian.PutUint64(raw[8:16], counts.rejected)
	binary.BigEndian.PutUint64(raw[16:], counts.negativeDuration)
	return tx.Bucket(storageMeta).Put(catalogCountsKey, raw[:])
}

// Build the derived index once for an existing conversation database, without
// reading, rewriting, or migrating any request or response bodies.
func initializeCatalogCounts(tx *bolt.Tx) error {
	if len(tx.Bucket(storageMeta).Get(catalogCountsKey)) == 24 {
		return nil
	}
	var counts catalogCounts
	err := tx.Bucket(summaries).ForEach(func(_, raw []byte) error {
		var row struct {
			Outcome    string  `json:"outcome"`
			DurationMS float64 `json:"duration_ms"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		counts.total++
		if row.Outcome == "rejected" {
			counts.rejected++
		}
		if row.DurationMS < 0 {
			counts.negativeDuration++
		}
		return nil
	})
	if err != nil {
		return err
	}
	return writeCatalogCounts(tx, counts)
}

func invalidateCatalog(tx *bolt.Tx) error {
	_, err := tx.Bucket(summaries).NextSequence()
	return err
}

func updateCatalogCounts(tx *bolt.Tx, before, after *Summary) error {
	counts := readCatalogCounts(tx)
	if before != nil {
		if counts.total == 0 || before.Outcome == "rejected" && counts.rejected == 0 || before.DurationMS < 0 && counts.negativeDuration == 0 {
			return errors.New("invalid catalog counts")
		}
		counts.total--
		if before.Outcome == "rejected" {
			counts.rejected--
		}
		if before.DurationMS < 0 {
			counts.negativeDuration--
		}
	}
	if after != nil {
		counts.total++
		if after.Outcome == "rejected" {
			counts.rejected++
		}
		if after.DurationMS < 0 {
			counts.negativeDuration++
		}
	}
	if err := writeCatalogCounts(tx, counts); err != nil {
		return err
	}
	return invalidateCatalog(tx)
}

func normalizeFilter(f Filter) Filter {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

func fastCatalogTotal(tx *bolt.Tx, f Filter) (int, bool) {
	if f.Query != "" || f.KeyID != "" || f.Alias != "" || f.Model != "" || f.Provider != "" || f.Session != "" || f.Reasoning != "" || !f.From.IsZero() || !f.To.IsZero() || f.MinDuration != 0 {
		return 0, false
	}
	counts := readCatalogCounts(tx)
	// Preserve the existing duration predicate for anomalous host timestamps.
	if counts.negativeDuration != 0 {
		return 0, false
	}
	if f.Outcome == "rejected" {
		if f.IgnoreRejected {
			return 0, true
		}
		return int(counts.rejected), true
	}
	if f.Outcome != "" {
		return 0, false
	}
	if f.IgnoreRejected {
		return int(counts.total - counts.rejected), true
	}
	return int(counts.total), true
}

// Retain at most one small response, never complete conversations. Copy on
// return prevents callers changing cached bytes. Image progress and settings
// writes do not invalidate an otherwise identical catalog.
type catalogCache struct {
	mu       sync.Mutex
	filter   Filter
	revision uint64
	body     []byte
}

const catalogCacheLimit = 1 << 20

func (s *Store) listJSON(f Filter) ([]byte, error) {
	f = normalizeFilter(f)
	var body []byte
	err := s.view(func(tx *bolt.Tx) error {
		revision := tx.Bucket(summaries).Sequence()
		s.catalog.mu.Lock()
		if s.catalog.body != nil && s.catalog.revision == revision && s.catalog.filter == f {
			body = append([]byte(nil), s.catalog.body...)
		}
		s.catalog.mu.Unlock()
		if body != nil {
			return nil
		}
		page, err := listCatalog(tx, f)
		if err != nil {
			return err
		}
		body, err = json.Marshal(page)
		if err != nil {
			return err
		}
		keyBytes := len(f.Query) + len(f.KeyID) + len(f.Alias) + len(f.Model) + len(f.Provider) + len(f.Outcome) + len(f.Session) + len(f.Reasoning)
		if len(body)+keyBytes <= catalogCacheLimit {
			s.catalog.mu.Lock()
			s.catalog.filter, s.catalog.revision = f, revision
			s.catalog.body = append([]byte(nil), body...)
			s.catalog.mu.Unlock()
		}
		return nil
	})
	return body, err
}
