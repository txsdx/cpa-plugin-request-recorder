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

	bolt "go.etcd.io/bbolt"
)

var conversationKeys = []byte("conversation_keys")
var conversationAliases = []byte("conversation_aliases")
var requestRecords = []byte("request_records")
var ErrConversationTooLarge = errors.New("会话历史超过 max_body_bytes，已保留前一完整快照")

func conversationMessages(r *Record) []Message {
	var messages []Message
	if r.ConversationHistory != "" && json.Unmarshal([]byte(r.ConversationHistory), &messages) == nil {
		for i := range messages {
			normalizeToolImages(&messages[i])
		}
		return messages
	}
	return Conversation(r.RequestBody, r.ResponseBody)
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
	var finalMessages []Message
	setHistory := func(messages []Message) error {
		resolveToolNames(messages)
		if conversationMessageBytes(messages) > s.historyLimit {
			return ErrConversationTooLarge
		}
		data, _ := json.Marshal(messages)
		r.ConversationHistory = string(data)
		finalMessages = messages
		return nil
	}
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
	session := clientConversationID(r)
	r.ClientSessionID = session
	parentSession, fork := parentConversationIdentity(r, session)
	r.IsFork = fork
	branchCandidate := parentSession != "" || fork
	scope := r.KeyID
	if r.CallerScope != "" {
		scope = "caller:" + r.CallerScope
	}
	var points []contextPoint
	var namespace string
	contextIndex := func() {
		if namespace == "" {
			namespace = scope + "/config:" + contextConfiguration(r)
			points = contextPoints(input)
		}
	}
	keys := []string{}
	var previous, parent *Record
	err := s.view(func(tx *bolt.Tx) error {
		decoded := map[string]*Record{}
		load := func(id string) (*Record, error) {
			if found, ok := decoded[id]; ok {
				return found, nil
			}
			raw := tx.Bucket(records).Get([]byte(id))
			if raw == nil {
				return nil, nil
			}
			found, err := decodeStoredRecord(tx, raw)
			if err == nil {
				decoded[id] = found
			}
			return found, err
		}
		id := r.ID
		old, err := load(id)
		if err != nil {
			return err
		}
		if old != nil {
			if old.RequestID == r.RequestID || old.RequestID == "" && old.ID == r.RequestID {
				previous = old
			}
		}
		if scope == "" {
			return nil
		}
		read := func(key string) (*Record, error) {
			value := tx.Bucket(conversationKeys).Get([]byte(key))
			if value == nil {
				return nil, nil
			}
			return load(string(value))
		}
		if session != "" {
			found, err := read(contextKey(scope, "session", session))
			if err != nil {
				return err
			}
			if found != nil && clientConversationID(found) == session {
				previous = found
			}
		}
		if previousResponseID != "" && (previous == nil || branchMayInherit(previous, r)) {
			found, err := read(contextKey(scope, "response", previousResponseID))
			if err != nil {
				return err
			}
			if found != nil && (previous == nil || found.ID != previous.ID) {
				foundSession := clientConversationID(found)
				distinct := session != "" && foundSession != "" && session != foundSession
				if distinct || branchCandidate && (found.Branch == nil || found.Branch.ParentSessionID != parentSession) {
					parent = found
					branchCandidate = true
					fork = true
				} else if previous == nil {
					previous, linked = found, true
				}
			}
		}
		if branchCandidate && (previous == nil || branchMayInherit(previous, r)) {
			if parentSession != "" {
				// An explicit parent wins over the global response index, which
				// can refer to an ancestor when the parent inherited that response.
				found, err := read(contextKey(scope, "session", parentSession))
				if err != nil {
					return err
				}
				parent = found
			}
			if parent != nil && (previous != nil && parent.ID == previous.ID || clientConversationID(parent) == session && session != "") {
				parent = nil
			}
		}
		if previous == nil && !branchCandidate {
			contextIndex()
			lookup := func(kind, digest string) (bool, error) {
				found, err := read(contextKey(namespace, kind, digest))
				if err != nil || found == nil {
					return false, err
				}
				if foundSession := clientConversationID(found); session != "" && foundSession != "" && foundSession != session {
					return false, nil
				}
				previous = found
				return true, nil
			}
			if len(points) > 0 {
				if found, err := lookup("input", points[len(points)-1].digest); err != nil || found {
					return err
				}
			}
			for i := len(points) - 1; i >= 0; i-- {
				if found, err := lookup("state", points[i].digest); err != nil || found {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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
		if previous.Branch != nil {
			branch := *previous.Branch
			r.Branch = &branch
		}
		r.InheritedResponseIDs = append([]string(nil), previous.InheritedResponseIDs...)
		r.IsFork = r.IsFork || previous.IsFork
		if session == "" {
			session = clientConversationID(previous)
			r.ClientSessionID = session
		}
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
				end := responseHistoryEnd(history, previousResponseID)
				if end > 0 {
					history = history[:end]
					linked = true
					input = withoutHistoryNotice(input)
				} else {
					history = nil
				}
			}
			messages := mergeConversation(history, input, output, session != "" || linked)
			if err := setHistory(messages); err != nil {
				return nil, err
			}
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
	if branchCandidate && (previous == nil || branchMayInherit(previous, r)) {
		branch := &ConversationBranch{Kind: "child", ParentSessionID: parentSession, ResponseID: previousResponseID}
		if fork {
			branch.Kind, r.IsFork = "fork", true
		}
		if parent != nil {
			branch.ParentConversationID = parent.ID
			if branch.ParentSessionID == "" {
				branch.ParentSessionID = clientConversationID(parent)
			}
		}
		inherited, status := inheritedBranchHistory(parent, input, previousResponseID)
		branch.Status, r.Branch = status, branch
		if len(inherited) > 0 {
			messages := mergeConversation(inherited, withoutHistoryNotice(input), output, status == "response")
			if err := setHistory(messages); err != nil {
				return nil, err
			}
			r.InheritedResponseIDs = inheritedResponseIDs(inherited)
		}
	}
	if r.ConversationHistory == "" {
		messages := append(input, output...)
		if err := setHistory(messages); err != nil {
			return nil, err
		}
	}
	if len(r.ConversationHistory) > s.historyLimit {
		return nil, ErrConversationTooLarge
	}
	updateConversationTotals(r, previous)
	if finalMessages == nil {
		finalMessages = conversationMessages(r)
	}
	steps := conversationSteps(finalMessages)
	r.ConversationSteps = &steps
	if scope != "" {
		inherited := map[string]bool{}
		for _, id := range r.InheritedResponseIDs {
			inherited[id] = true
		}
		for _, message := range finalMessages {
			if message.ResponseID != "" && !inherited[message.ResponseID] {
				keys = append(keys, contextKey(scope, "response", message.ResponseID))
			}
		}
		if session != "" {
			keys = append(keys, contextKey(scope, "session", session))
		} else {
			contextIndex()
			if len(points) > 0 {
				keys = append(keys, contextKey(namespace, "input", points[len(points)-1].digest))
			}
			states := contextPoints(finalMessages)
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
