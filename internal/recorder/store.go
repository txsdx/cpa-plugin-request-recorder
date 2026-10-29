package recorder

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrNotFound = errors.New("record not found")
var records = []byte("records")
var summaries = []byte("summaries")
var ordering = []byte("time")
var aliases = []byte("aliases")
var panelAliases = []byte("panel_aliases")
var traces = []byte("traces")
var imageAssets = []byte("images")
var imageJobs = []byte("image_jobs")

var storageMeta = []byte("storage_meta")

type mutationJournal struct {
	operations []func(*bolt.Tx) error
	bytes      int
	overflow   bool
}
type Store struct {
	mu                   sync.RWMutex
	writes               sync.Mutex
	maintenance          sync.Mutex
	wg                   sync.WaitGroup
	closing              bool
	running              bool
	journal              *mutationJournal
	db                   *bolt.DB
	limit                int64
	historyLimit         int
	lastMaintenanceError string
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	db.AllocSize = 64 << 10
	s := &Store{db: db, limit: 1 << 30, historyLimit: 64 << 20}
	err = db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(records) != nil && tx.Bucket(records).Stats().KeyN > 0 && (tx.Bucket(storageMeta) == nil || string(tx.Bucket(storageMeta).Get([]byte("record_layout"))) != "conversation-v1") {
			return errors.New("旧数据库格式不再支持，请停止 CPA 并移走旧数据库后重启")
		}
		for _, name := range [][]byte{records, summaries, ordering, aliases, panelAliases, traces, imageAssets, imageJobs, bodyChunks, bodyCounts, storageMeta, conversationKeys, conversationAliases, requestRecords} {
			if _, e := tx.CreateBucketIfNotExists(name); e != nil {
				return e
			}
		}
		return tx.Bucket(storageMeta).Put([]byte("record_layout"), []byte("conversation-v1"))
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	s.maintenance.Lock()
	s.closing = true
	s.maintenance.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
func orderKey(r Summary) []byte { return []byte(fmt.Sprintf("%020d:%s", r.StartedAt.UnixNano(), r.ID)) }
func (s *Store) Save(input *Record) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	originalID, originalRequestID := input.ID, input.RequestID
	if originalRequestID == "" {
		originalRequestID = originalID
	}
	snapshot := *input
	r := &snapshot
	keys, err := s.prepareConversation(r)
	if err != nil {
		return err
	}
	stored, chunks := prepareStoredRecord(r)
	raw, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	sum, err := json.Marshal(stored.Summary)
	if err != nil {
		return err
	}
	// Compress before the write transaction; readers remain available.
	encoded := map[string][]byte{}
	err = s.view(func(tx *bolt.Tx) error {
		for id, body := range chunks {
			if tx.Bucket(bodyChunks).Get([]byte(id)) == nil {
				data, err := encodeBodyChunk(body)
				if err != nil {
					return err
				}
				encoded[id] = data
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	cost := len(raw)
	for _, body := range recordBodyFields(r) {
		cost += len(*body)
	}
	r = &stored.Record // immutable metadata captured for compaction replay
	err = s.updateLocked(cost, func(tx *bolt.Tx) error {
		id := []byte(r.ID)
		oldIDs, err := storedChunkIDs(tx.Bucket(records).Get(id))
		if err != nil {
			return err
		}
		for chunkID, body := range chunks {
			key := []byte(chunkID)
			if tx.Bucket(bodyChunks).Get(key) == nil {
				data := encoded[chunkID]
				if data == nil {
					data, err = encodeBodyChunk(body) // pruned between read/write transactions
					if err != nil {
						return err
					}
				}
				if err := tx.Bucket(bodyChunks).Put(key, data); err != nil {
					return err
				}
			}
			if !oldIDs[chunkID] {
				var count uint64
				if value := tx.Bucket(bodyCounts).Get(key); value != nil {
					if len(value) != 8 {
						return errors.New("invalid body reference count")
					}
					count = binary.BigEndian.Uint64(value)
				}
				var value [8]byte
				binary.BigEndian.PutUint64(value[:], count+1)
				if err := tx.Bucket(bodyCounts).Put(key, value[:]); err != nil {
					return err
				}
			}
		}
		for chunkID := range oldIDs {
			if _, exists := chunks[chunkID]; !exists {
				if err := releaseBodyChunk(tx, chunkID); err != nil {
					return err
				}
			}
		}
		if r.TraceID != "" {
			if err := tx.Bucket(traces).Put([]byte(r.TraceID+"\x00"+r.ID), id); err != nil {
				return err
			}
		}
		if old := tx.Bucket(summaries).Get(id); old != nil {
			var o Summary
			if err := json.Unmarshal(old, &o); err != nil {
				return err
			}
			if err := tx.Bucket(ordering).Delete(orderKey(o)); err != nil {
				return err
			}
			if o.RequestID != r.RequestID && o.RequestID != "" {
				if err := tx.Bucket(requestRecords).Delete([]byte(o.RequestID)); err != nil {
					return err
				}
			}
			if o.TraceID != r.TraceID {
				if err := tx.Bucket(traces).Delete([]byte(o.TraceID + "\x00" + o.ID)); err != nil {
					return err
				}
			}
		}
		if err := tx.Bucket(imageJobs).Put(id, []byte{1}); err != nil {
			return err
		}
		if err := tx.Bucket(records).Put(id, raw); err != nil {
			return err
		}
		if err := tx.Bucket(summaries).Put(id, sum); err != nil {
			return err
		}
		if originalID != r.ID {
			if old := tx.Bucket(summaries).Get([]byte(originalID)); old != nil {
				var obsolete Summary
				if err := json.Unmarshal(old, &obsolete); err != nil {
					return err
				}
				if obsolete.RequestID == originalRequestID && (obsolete.KeyID == "" || obsolete.KeyID == r.KeyID) {
					if err := mergeImageInventory(tx, []byte(originalID), id); err != nil {
						return err
					}
					if err := removeStoredRecord(tx, []byte(originalID), obsolete); err != nil {
						return err
					}
				}
			}
		}
		if err := saveConversationKeys(tx, r, keys); err != nil {
			return err
		}
		return tx.Bucket(ordering).Put(orderKey(r.Summary), id)
	})
	if err == nil {
		*input = snapshot
	}
	return err
}
func (s *Store) Get(id string) (*Record, error) {
	var r *Record
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket(records).Get([]byte(id))
		if b == nil {
			if actual := tx.Bucket(requestRecords).Get([]byte(id)); actual != nil {
				id = string(actual)
				b = tx.Bucket(records).Get(actual)
			}
		}
		if b == nil {
			return ErrNotFound
		}
		var err error
		r, err = decodeStoredRecord(tx, b)
		if err != nil {
			return err
		}
		if raw := tx.Bucket(imageAssets).Get([]byte(id)); raw != nil {
			if err := json.Unmarshal(raw, &r.Images); err != nil {
				return err
			}
		}
		r.KeyAlias = resolvedAlias(tx, r.KeyID)
		return nil
	})
	return r, err
}

type Filter struct {
	Query, KeyID, Alias, Model, Provider, Outcome, Session, Reasoning string
	From, To                                                          time.Time
	MinDuration                                                       float64
	Offset, Limit                                                     int
	IgnoreRejected                                                    bool
}
type Page struct {
	Items  []Summary `json:"items"`
	Total  int       `json:"total"`
	Offset int       `json:"offset"`
	Limit  int       `json:"limit"`
}

func (s *Store) List(f Filter) (Page, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	p := Page{Items: []Summary{}, Offset: f.Offset, Limit: f.Limit}
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(ordering).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var r Summary
			if err := json.Unmarshal(tx.Bucket(summaries).Get(v), &r); err != nil {
				return err
			}
			r.KeyAlias = resolvedAlias(tx, r.KeyID)
			if f.IgnoreRejected && r.Outcome == "rejected" {
				continue
			}
			if !f.From.IsZero() && r.StartedAt.Before(f.From) {
				continue
			}
			if !f.To.IsZero() && r.StartedAt.After(f.To) {
				continue
			}
			if f.KeyID != "" && r.KeyID != f.KeyID || f.Outcome != "" && r.Outcome != f.Outcome || f.Session != "" && !matchesConversationSession(r, f.Session) || r.DurationMS < f.MinDuration {
				continue
			}
			match := true
			for _, pair := range [][2]string{{r.KeyAlias, f.Alias}, {r.Model + " " + r.RequestedModel + " " + r.ResponseModel, f.Model}, {r.Provider, f.Provider}, {r.ReasoningEffort, f.Reasoning}} {
				if !contains(pair[0], pair[1]) {
					match = false
					break
				}
			}
			if !match || !contains(strings.Join([]string{r.ID, r.TraceID, r.KeyAlias, r.KeyHint, r.KeyID, r.CallerScope, r.Model, r.RequestedModel, r.ResponseModel, r.Provider, r.SessionID, r.ClientSessionID, r.UserAgent, r.LatestPrompt, r.LatestResponse}, " "), f.Query) {
				continue
			}
			if p.Total >= f.Offset && len(p.Items) < f.Limit {
				p.Items = append(p.Items, r)
			}
			p.Total++
		}
		return nil
	})
	return p, err
}
func contains(s, q string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(q)) }
func (s *Store) SetAlias(id, alias string) error {
	_, decodeErr := hex.DecodeString(id)
	if len(id) != 64 || decodeErr != nil {
		return errors.New("key_id must be a SHA-256 fingerprint")
	}
	if len(alias) > 120 {
		return errors.New("alias too long")
	}
	return s.update(0, func(tx *bolt.Tx) error {
		if alias == "" {
			return tx.Bucket(aliases).Delete([]byte(id))
		}
		return tx.Bucket(aliases).Put([]byte(id), []byte(alias))
	})
}
func (s *Store) Prune(days, max int) error {
	cutoff := time.Now().AddDate(0, 0, -days)
	return s.update(4096, func(tx *bolt.Tx) error {
		count := tx.Bucket(ordering).Stats().KeyN
		for {
			_, id := tx.Bucket(ordering).Cursor().First()
			if id == nil {
				break
			}
			id = append([]byte(nil), id...)
			var summary Summary
			if err := json.Unmarshal(tx.Bucket(summaries).Get(id), &summary); err != nil {
				return err
			}
			if (days <= 0 || !summary.StartedAt.Before(cutoff)) && (max <= 0 || count <= max) {
				break
			}
			if err := removeStoredRecord(tx, id, summary); err != nil {
				return err
			}
			count--
		}
		return nil
	})
}

// ByTrace uses a dedicated index; a trace may contain several nested executions.
func (s *Store) ByTrace(trace string) ([]Summary, error) {
	out := []Summary{}
	if trace == "" {
		return out, nil
	}
	err := s.view(func(tx *bolt.Tx) error {
		prefix := []byte(trace + "\x00")
		c := tx.Bucket(traces).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var r Summary
			if raw := tx.Bucket(summaries).Get(v); raw != nil {
				if err := json.Unmarshal(raw, &r); err != nil {
					return err
				}
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// Image metadata is separate from large request bodies. Download progress updates
// cannot overwrite concurrent usage updates or rewrite multi-MiB bodies.
func (s *Store) GetImages(id string) ([]ImageAsset, error) {
	out := []ImageAsset{}
	err := s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		if raw := tx.Bucket(imageAssets).Get([]byte(id)); raw != nil {
			return json.Unmarshal(raw, &out)
		}
		return nil
	})
	return out, err
}
func (s *Store) SetImages(id string, assets []ImageAsset) error {
	raw, err := json.Marshal(assets)
	if err != nil {
		return err
	}
	return s.update(len(raw), func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return tx.Bucket(imageAssets).Put([]byte(id), raw)
	})
}
func (s *Store) ImageReferences() (map[string]bool, error) {
	refs := map[string]bool{}
	err := s.view(func(tx *bolt.Tx) error {
		return tx.Bucket(imageAssets).ForEach(func(_, raw []byte) error {
			var images []ImageAsset
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
func (s *Store) PendingImageRecords(limit int) ([]string, error) {
	ids := []string{}
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(imageJobs).Cursor()
		for k, _ := c.First(); k != nil && len(ids) < limit; k, _ = c.Next() {
			ids = append(ids, string(k))
		}
		return nil
	})
	return ids, err
}
func (s *Store) QueueImages(id string) error {
	queued := false
	if err := s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(records).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		queued = tx.Bucket(imageJobs).Get([]byte(id)) != nil
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
		return tx.Bucket(imageJobs).Put([]byte(id), []byte{1})
	})
}
func (s *Store) FinishImages(id string) error {
	return s.update(0, func(tx *bolt.Tx) error {
		return tx.Bucket(imageJobs).Delete([]byte(id))
	})
}

func (s *Store) ImageFailed(recordID, id, message string) error {
	return s.update(0, func(tx *bolt.Tx) error {
		b := tx.Bucket(imageAssets)
		raw := b.Get([]byte(recordID))
		if raw == nil {
			return ErrNotFound
		}
		var assets []ImageAsset
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

// Explicit recorder aliases take precedence over the last CPAMC snapshot.
func resolvedAlias(tx *bolt.Tx, id string) string {
	if value := tx.Bucket(aliases).Get([]byte(id)); len(value) > 0 {
		return string(value)
	}
	return string(tx.Bucket(panelAliases).Get([]byte(id)))
}
func (s *Store) SyncPanelAliases(input map[string]string) error {
	values := make(map[string]string, len(input))
	for k, v := range input {
		values[k] = v
	}
	if len(values) > 10000 {
		return errors.New("too many aliases")
	}
	for id, alias := range values {
		if !validSHA256(id) || len(alias) > 1024 || strings.TrimSpace(alias) == "" {
			return errors.New("invalid alias: lowercase SHA-256 key_id and nonempty alias up to 1024 bytes required")
		}
	}
	cost := 4096
	for k, v := range values {
		cost += len(k) + len(v)
	}
	return s.update(cost, func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(panelAliases); err != nil {
			return err
		}
		b, err := tx.CreateBucket(panelAliases)
		if err != nil {
			return err
		}
		for id, alias := range values {
			if err := b.Put([]byte(id), []byte(strings.TrimSpace(alias))); err != nil {
				return err
			}
		}
		return nil
	})
}
