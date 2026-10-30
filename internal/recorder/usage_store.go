package recorder

import (
	"encoding/json"
	"errors"

	bolt "go.etcd.io/bbolt"
)

var errUsageStillCurrent = errors.New("usage belongs to the current execution")

// Update only cumulative accounting for a retired execution. The latest request,
// response, usage attempts, key identity and body manifests remain untouched.
// current asks the caller to use normal per-request usage handling instead.
func (s *Store) addDelayedUsage(conversationID, executionID string, delta int64, first bool) (current bool, err error) {
	err = s.update(4096, func(tx *bolt.Tx) error {
		id := []byte(conversationID)
		raw := tx.Bucket(records).Get(id)
		if raw == nil {
			return ErrNotFound
		}
		var record storedRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.RequestID == executionID {
			return errUsageStillCurrent
		}
		totals := conversationTotals(&record.Record)
		totals.TotalTokens += delta
		if first {
			totals.TokenRequests++
		}
		record.ConversationTotals = &totals
		next, err := json.Marshal(&record)
		if err != nil {
			return err
		}
		summary, err := json.Marshal(record.Summary)
		if err != nil {
			return err
		}
		if err = tx.Bucket(records).Put(id, next); err != nil {
			return err
		}
		if err := tx.Bucket(summaries).Put(id, summary); err != nil {
			return err
		}
		return invalidateCatalog(tx)
	})
	if errors.Is(err, errUsageStillCurrent) {
		return true, nil
	}
	return false, err
}
