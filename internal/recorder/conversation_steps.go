package recorder

import (
	"encoding/json"

	bolt "go.etcd.io/bbolt"
)

// Count replay steps, grouping adjacent text/thought/image blocks of the same
// speaker. Each tool call/result is a separate step; instructions/notices and
// alternate choices do not add dialogue steps.
func conversationSteps(messages []Message) int {
	steps := 0
	var previous *Message
	for i := range messages {
		m := &messages[i]
		if m.Choice > 0 || m.Kind == "notice" {
			continue
		}
		if m.Role == "system" || m.Role == "developer" {
			previous = nil
			continue
		}
		if m.Content == "" && len(m.Images) == 0 && m.Kind != "tool_call" && m.Kind != "tool_result" {
			continue
		}
		tool := m.Kind == "tool_call" || m.Kind == "tool_result"
		same := previous != nil && previous.Role == m.Role && previous.ResponseID == m.ResponseID
		if tool {
			same = same && m.ToolCallID != "" && previous.Kind == m.Kind && previous.ToolCallID == m.ToolCallID
		} else if previous != nil && (previous.Kind == "tool_call" || previous.Kind == "tool_result") {
			same = false
		}
		if !same {
			steps++
		}
		previous = m
	}
	return steps
}

// Backfill only a viewed snapshot, without re-reading/writing its large bodies.
// A newer request must never receive a count computed from an older history.
func (s *Store) fillConversationSteps(id, requestID string, steps int) error {
	return s.update(4096, func(tx *bolt.Tx) error {
		key := []byte(id)
		raw := tx.Bucket(records).Get(key)
		if raw == nil {
			return ErrNotFound
		}
		var row storedRecord
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.RequestID != requestID || row.ConversationSteps != nil {
			return nil
		}
		row.ConversationSteps = &steps
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		summary, err := json.Marshal(row.Summary)
		if err != nil {
			return err
		}
		if err := tx.Bucket(records).Put(key, raw); err != nil {
			return err
		}
		if err := tx.Bucket(summaries).Put(key, summary); err != nil {
			return err
		}
		return invalidateCatalog(tx)
	})
}
