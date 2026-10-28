package recorder

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var conversationKeys = []byte("conversation_keys")
var conversationAliases = []byte("conversation_aliases")
var requestRecords = []byte("request_records")
var ErrConversationTooLarge = errors.New("会话历史超过 max_body_bytes，已保留前一完整快照")

func conversationMessages(r *Record) []Message {
	var messages []Message
	if r.ConversationHistory != "" && json.Unmarshal([]byte(r.ConversationHistory), &messages) == nil {
		return messages
	}
	return Conversation(r.RequestBody, r.ResponseBody)
}

// Use explicit client thread IDs, not the host's cache/routing/execution IDs.
func clientConversationID(r *Record) string {
	for _, name := range []string{"Session_id", "X-Session-ID", "X-Conversation-ID", "Conversation-ID", "X-Thread-ID", "Thread-ID"} {
		if value := strings.TrimSpace(r.RequestHeaders.Get(name)); value != "" && len(value) <= 256 {
			return "client:" + value
		}
	}
	body := decode(r.RequestBody)
	for _, candidate := range []map[string]any{body, obj(body["metadata"])} {
		for _, name := range []string{"conversation_id", "thread_id", "session_id"} {
			if value := str(candidate[name]); value != "" && len(value) <= 256 {
				return "client:" + value
			}
		}
	}
	if value := str(obj(body["conversation"])["id"]); value != "" && len(value) <= 256 {
		return "client:" + value
	}
	return ""
}

func writeConversationField(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	h.Write(length[:])
	_, _ = io.WriteString(h, value)
}
func contextMessageID(m Message) string {
	h := sha256.New()
	content := m.Content
	name := m.Name
	if m.Kind == "tool_result" && m.ToolCallID != "" {
		name = "" // OpenAI clients may omit the optional result name.
	}
	if m.Kind == "tool_call" || m.Kind == "tool_result" {
		var value any
		if json.Unmarshal([]byte(content), &value) == nil {
			compact, _ := json.Marshal(value)
			content = string(compact)
		}
	}
	for _, value := range []string{m.Role, m.Kind, name, m.ToolCallID, content} {
		writeConversationField(h, value)
	}
	for _, image := range m.Images {
		writeConversationField(h, image.URL)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type contextPoint struct {
	digest string
	end    int
	role   string
}

func contextPoints(messages []Message) []contextPoint {
	h := sha256.New()
	points := []contextPoint{}
	for i, m := range messages {
		// Clients commonly omit past reasoning; retain it in the archive while
		// matching only the observable conversation context they actually resend.
		if m.Kind == "reasoning" || m.Kind == "notice" || m.Choice > 0 || m.Role == "system" || m.Role == "developer" {
			continue
		}
		writeConversationField(h, contextMessageID(m))
		points = append(points, contextPoint{hex.EncodeToString(h.Sum(nil)), i + 1, m.Role})
	}
	return points
}
func contextKey(scope, kind, digest string) string { return scope + "\x00" + kind + ":" + digest }

func mergeConversation(previous, input, output []Message, explicit bool) []Message {
	var config []Message
	strip := func(messages []Message) []Message {
		result := []Message{}
		for _, message := range messages {
			if message.Role == "system" || message.Role == "developer" {
				continue
			}
			result = append(result, message)
		}
		return result
	}
	for _, message := range input {
		if message.Role == "system" || message.Role == "developer" {
			config = append(config, message)
		}
	}
	if len(config) == 0 {
		for _, message := range previous {
			if message.Role == "system" || message.Role == "developer" {
				config = append(config, message)
			}
		}
	}
	previous, input = strip(previous), strip(input)
	oldPoints, newPoints := contextPoints(previous), contextPoints(input)
	shared := 0
	for shared < len(oldPoints) && shared < len(newPoints) && oldPoints[shared].digest == newPoints[shared].digest {
		shared++
	}
	merged := append([]Message{}, config...)
	if shared == 0 {
		if explicit {
			merged = append(merged, previous...)
		}
		merged = append(merged, input...)
	} else {
		merged = append(merged, previous[:oldPoints[shared-1].end]...)
		merged = append(merged, input[newPoints[shared-1].end:]...)
	}
	return append(merged, output...)
}
func contextConfiguration(r *Record) string {
	h := sha256.New()
	for _, message := range requestMessages(r.RequestBody) {
		if message.Role == "system" || message.Role == "developer" {
			writeConversationField(h, contextMessageID(message))
		}
	}
	body := decode(r.RequestBody)
	for _, field := range []string{"tools", "functions", "tool_choice", "response_format"} {
		if value := body[field]; value != nil {
			data, _ := json.Marshal(value)
			writeConversationField(h, field)
			writeConversationField(h, string(data))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Called under the store writer lock so choosing and replacing a conversation
// head cannot race another turn. No fuzzy text or model/key-only grouping.
func (s *Store) prepareConversation(r *Record) ([]string, error) {
	if r.RequestID == "" {
		r.RequestID = r.ID
	}
	input, output := requestMessages(r.RequestBody), responseMessages(r.ResponseBody)
	responseID := str(decode(r.ResponseBody)["id"])
	for i := range output {
		output[i].ResponseID = responseID
		output[i].Source = "response"
	}
	for i := range input {
		input[i].Source = "request"
	}
	previousResponseID := str(decode(r.RequestBody)["previous_response_id"])
	linked := false
	points := contextPoints(input)
	session := clientConversationID(r)
	scope := r.KeyID
	if r.CallerScope != "" {
		scope = "caller:" + r.CallerScope
	}
	namespace := scope + "/config:" + contextConfiguration(r)
	keys := []string{}
	var previous *Record
	var lookupErr error
	err := s.view(func(tx *bolt.Tx) error {
		id := r.ID
		if raw := tx.Bucket(records).Get([]byte(id)); raw != nil {
			old, err := decodeStoredRecord(tx, raw)
			if err != nil {
				return err
			}
			if old.RequestID == r.RequestID || old.RequestID == "" && old.ID == r.RequestID {
				previous = old
			}
		}
		if scope == "" {
			return nil
		}
		lookup := func(key string) bool {
			value := tx.Bucket(conversationKeys).Get([]byte(key))
			if value == nil {
				return false
			}
			raw := tx.Bucket(records).Get(value)
			if raw == nil {
				return false
			}
			found, err := decodeStoredRecord(tx, raw)
			if err != nil {
				lookupErr = err
				return false
			}
			previous = found
			return true
		}
		if previousResponseID != "" && lookup(contextKey(scope, "response", previousResponseID)) {
			linked = true
			return nil
		}
		if session != "" {
			lookup(contextKey(scope, "session", session))
			return nil
		}
		if len(points) > 0 && lookup(contextKey(namespace, "input", points[len(points)-1].digest)) {
			return nil
		}
		for i := len(points) - 1; i >= 0; i-- {
			if lookup(contextKey(namespace, "state", points[i].digest)) {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	if previous != nil && previous.RequestID != r.RequestID && !r.StartedAt.IsZero() && previous.StartedAt.After(r.StartedAt) {
		*r = *previous
		var aliases []string
		err := s.view(func(tx *bolt.Tx) error {
			return json.Unmarshal(tx.Bucket(conversationAliases).Get([]byte(r.ID)), &aliases)
		})
		return aliases, err
	}
	if previous != nil {
		r.ID = previous.ID
		r.ConversationStartedAt = previous.ConversationStartedAt
		if r.ConversationStartedAt.IsZero() {
			r.ConversationStartedAt = previous.StartedAt
		}
		r.ConversationTurns = previous.ConversationTurns
		if previous.RequestID == r.RequestID {
			r.ConversationHistory = previous.ConversationHistory
		} else {
			history := conversationMessages(previous)
			if previousResponseID != "" {
				end := 0
				for i, message := range history {
					if message.ResponseID == previousResponseID {
						end = i + 1
					}
				}
				if end > 0 {
					history = history[:end]
					linked = true
					cleaned := input[:0]
					for _, message := range input {
						if message.Kind != "notice" {
							cleaned = append(cleaned, message)
						}
					}
					input = cleaned
				} else {
					history = nil
				}
			}
			messages := mergeConversation(history, input, output, session != "" || linked)
			resolveToolNames(messages)
			if conversationMessageBytes(messages) > s.historyLimit {
				return nil, ErrConversationTooLarge
			}
			data, _ := json.Marshal(messages)
			r.ConversationHistory = string(data)
			r.ConversationTurns++
		}
	} else {
		if scope != "" {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return nil, err
			}
			r.ID = "conv_" + hex.EncodeToString(random[:])
		}
		r.ConversationStartedAt = r.StartedAt
		r.ConversationTurns = 1
	}
	if r.ConversationHistory == "" {
		messages := append(input, output...)
		resolveToolNames(messages)
		if conversationMessageBytes(messages) > s.historyLimit {
			return nil, ErrConversationTooLarge
		}
		data, _ := json.Marshal(messages)
		r.ConversationHistory = string(data)
	}
	if len(r.ConversationHistory) > s.historyLimit {
		return nil, ErrConversationTooLarge
	}
	updateConversationTotals(r, previous)
	if scope != "" {
		for _, message := range conversationMessages(r) {
			if message.ResponseID != "" {
				keys = append(keys, contextKey(scope, "response", message.ResponseID))
			}
		}
		if session != "" {
			keys = append(keys, contextKey(scope, "session", session))
		} else {
			if len(points) > 0 {
				keys = append(keys, contextKey(namespace, "input", points[len(points)-1].digest))
			}
			states := contextPoints(conversationMessages(r))
			for i, point := range states {
				if i+1 == len(states) || states[i+1].role != point.role {
					kind := "input"
					if point.role == "assistant" {
						kind = "state"
					}
					keys = append(keys, contextKey(namespace, kind, point.digest))
				}
			}
		}
	}
	unique := keys[:0]
	seen := map[string]bool{}
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			unique = append(unique, key)
		}
	}
	return unique, nil
}

func clearConversationKeys(tx *bolt.Tx, id []byte) error {
	var keys []string
	if raw := tx.Bucket(conversationAliases).Get(id); raw != nil {
		if err := json.Unmarshal(raw, &keys); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if string(tx.Bucket(conversationKeys).Get([]byte(key))) == string(id) {
			if err := tx.Bucket(conversationKeys).Delete([]byte(key)); err != nil {
				return err
			}
		}
	}
	return tx.Bucket(conversationAliases).Delete(id)
}
func saveConversationKeys(tx *bolt.Tx, r *Record, keys []string) error {
	id := []byte(r.ID)
	if err := clearConversationKeys(tx, id); err != nil {
		return err
	}
	for _, key := range keys {
		if err := tx.Bucket(conversationKeys).Put([]byte(key), id); err != nil {
			return err
		}
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	if err := tx.Bucket(conversationAliases).Put(id, data); err != nil {
		return err
	}
	if r.RequestID != "" {
		return tx.Bucket(requestRecords).Put([]byte(r.RequestID), id)
	}
	return nil
}

func conversationMessageBytes(messages []Message) int {
	n := 0
	for _, message := range messages {
		n += len(message.Content) + len(message.Name) + len(message.ToolCallID) + 128
		for _, image := range message.Images {
			n += len(image.URL)
		}
	}
	return n
}

func mergeImageInventory(tx *bolt.Tx, from, to []byte) error {
	var previous, current []ImageAsset
	if raw := tx.Bucket(imageAssets).Get(from); raw != nil {
		if err := json.Unmarshal(raw, &previous); err != nil {
			return err
		}
	}
	if raw := tx.Bucket(imageAssets).Get(to); raw != nil {
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, asset := range current {
		seen[asset.ID] = true
	}
	for _, asset := range previous {
		if !seen[asset.ID] {
			current = append(current, asset)
			seen[asset.ID] = true
		}
	}
	if len(current) == 0 {
		return nil
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return tx.Bucket(imageAssets).Put(to, raw)
}
