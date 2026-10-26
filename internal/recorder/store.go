package recorder

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrNotFound = errors.New("record not found")
var records = []byte("records")
var summaries = []byte("summaries")
var ordering = []byte("time")
var aliases = []byte("aliases")
var traces = []byte("traces")

type Store struct{ db *bolt.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{records, summaries, ordering, aliases, traces} {
			if _, e := tx.CreateBucketIfNotExists(name); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error   { return s.db.Close() }
func orderKey(r Summary) []byte { return []byte(fmt.Sprintf("%020d:%s", r.StartedAt.UnixNano(), r.ID)) }
func (s *Store) Save(r *Record) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		id := []byte(r.ID)
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
			if o.TraceID != r.TraceID {
				if err := tx.Bucket(traces).Delete([]byte(o.TraceID + "\x00" + o.ID)); err != nil {
					return err
				}
			}
		}
		// Aliases are joined at read time, so edits apply to historical records too.
		r.KeyAlias = ""
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		sum, err := json.Marshal(r.Summary)
		if err != nil {
			return err
		}
		if err = tx.Bucket(records).Put(id, raw); err != nil {
			return err
		}
		if err = tx.Bucket(summaries).Put(id, sum); err != nil {
			return err
		}
		return tx.Bucket(ordering).Put(orderKey(r.Summary), id)
	})
}
func (s *Store) Get(id string) (*Record, error) {
	var r Record
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(records).Get([]byte(id))
		if b == nil {
			return ErrNotFound
		}
		if e := json.Unmarshal(b, &r); e != nil {
			return e
		}
		r.KeyAlias = string(tx.Bucket(aliases).Get([]byte(r.KeyID)))
		return nil
	})
	return &r, err
}

type Filter struct {
	Query, KeyID, Alias, IP, Model, Provider, Outcome, Session, Reasoning string
	From, To                                                              time.Time
	MinDuration                                                           float64
	Offset, Limit                                                         int
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
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(ordering).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var r Summary
			if err := json.Unmarshal(tx.Bucket(summaries).Get(v), &r); err != nil {
				return err
			}
			r.KeyAlias = string(tx.Bucket(aliases).Get([]byte(r.KeyID)))
			if !f.From.IsZero() && r.StartedAt.Before(f.From) {
				continue
			}
			if !f.To.IsZero() && r.StartedAt.After(f.To) {
				continue
			}
			if f.KeyID != "" && r.KeyID != f.KeyID || f.Outcome != "" && r.Outcome != f.Outcome || f.Session != "" && r.SessionID != f.Session || r.DurationMS < f.MinDuration {
				continue
			}
			match := true
			for _, pair := range [][2]string{{r.KeyAlias, f.Alias}, {r.ClientIP, f.IP}, {r.Model + " " + r.RequestedModel + " " + r.ResponseModel, f.Model}, {r.Provider, f.Provider}, {r.ReasoningEffort, f.Reasoning}} {
				if !contains(pair[0], pair[1]) {
					match = false
					break
				}
			}
			if !match || !contains(strings.Join([]string{r.ID, r.TraceID, r.KeyAlias, r.KeyHint, r.KeyID, r.CallerScope, r.ClientIP, r.Model, r.RequestedModel, r.ResponseModel, r.Provider, r.SessionID, r.UserAgent}, " "), f.Query) {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		if alias == "" {
			return tx.Bucket(aliases).Delete([]byte(id))
		}
		return tx.Bucket(aliases).Put([]byte(id), []byte(alias))
	})
}
func (s *Store) Prune(days, max int) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(ordering)
		count := b.Stats().KeyN
		cutoff := time.Now().AddDate(0, 0, -days)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r Summary
			if e := json.Unmarshal(tx.Bucket(summaries).Get(v), &r); e != nil {
				return e
			}
			if (days <= 0 || !r.StartedAt.Before(cutoff)) && (max <= 0 || count <= max) {
				break
			}
			id := append([]byte(nil), v...)
			if err := tx.Bucket(traces).Delete([]byte(r.TraceID + "\x00" + r.ID)); err != nil {
				return err
			}
			if e := tx.Bucket(records).Delete(id); e != nil {
				return e
			}
			if e := tx.Bucket(summaries).Delete(id); e != nil {
				return e
			}
			if e := c.Delete(); e != nil {
				return e
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
	err := s.db.View(func(tx *bolt.Tx) error {
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

func (s *Store) PendingLogs() ([]Summary, error) {
	out := []Summary{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(summaries).ForEach(func(_, v []byte) error {
			var r Summary
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.UpstreamState == "pending" && len(out) < 4096 {
				out = append(out, r)
			}
			return nil
		})
	})
	return out, err
}
