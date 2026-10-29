package recorder

import (
	"encoding/json"
	"strings"

	bolt "go.etcd.io/bbolt"
)

func conversationBody(r *Record) map[string]any {
	body := decode(r.RequestBody)
	if nested := obj(body["request"]); nested != nil {
		return nested
	}
	return body
}

func codexTurnMetadata(r *Record) map[string]any {
	if raw := conversationHeader(r.RequestHeaders, "X-Codex-Turn-Metadata"); len(raw) <= 16<<10 {
		return decode(raw)
	}
	return nil
}

func forkParentField(body, turn map[string]any) string {
	for _, candidate := range []map[string]any{turn, body, obj(body["metadata"]), obj(body["extra_body"])} {
		if id := sessionField(candidate, "forked_from_thread_id", "forked_from_id", "previous_session_id", "previousSessionId"); id != "" {
			return id
		}
		for _, name := range []string{"forkSource", "fork_source"} {
			if id := sessionField(obj(candidate[name]), "sessionId", "session_id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// ParentSessionID (also displayed by Billing) declares a relationship, not a
// snapshot boundary. It must never authorize copying a parent's entire head.
func parentConversationIdentity(r *Record, child string) (parent string, fork bool) {
	body, turn := conversationBody(r), codexTurnMetadata(r)
	metadata := obj(body["metadata"])
	claude := decode(str(metadata["user_id"]))
	fork = r.IsFork && child != ""
	if parent = forkParentField(body, turn); parent != "" {
		fork = true
	}
	if parent == "" {
		parent = sessionHeader(r.RequestHeaders, "X-Codex-Parent-Thread-Id", "X-Parent-Session-ID", "X-Parent-Conversation-ID", "X-Parent-Thread-ID")
	}
	if parent == "" {
		for _, candidate := range []map[string]any{turn, body, metadata, obj(body["extra_body"]), claude} {
			parent = sessionField(candidate, "parent_session_id", "parentSessionId", "parent_thread_id", "parentThreadId", "parent_conversation_id", "parentConversationId")
			if parent != "" {
				break
			}
		}
	}
	if parent == "" && strings.HasPrefix(child, "client:claude:") {
		agent := sessionHeader(r.RequestHeaders, "X-Claude-Code-Parent-Agent-Id")
		if agent == "" {
			agent = sessionField(claude, "parent_agent_id")
		}
		if agent == "" {
			agent = sessionField(metadata, "parent_agent_id", "parentAgentId")
		}
		if index := strings.Index(child, ":agent:"); index >= 0 && agent != "" && agent != "main" {
			parent = strings.TrimPrefix(child[:index], "client:") + ":agent:" + agent
		}
	}
	if parent == "" {
		if child != "" || hostConversationID(r.ParentSessionID) != "" {
			parent = sessionValue(r.ParentSessionID)
		}
	}
	if parent == "" {
		if index := strings.Index(child, ":agent:"); index >= 0 {
			parent = strings.TrimPrefix(child[:index], "client:")
		} else {
			sid := sessionHeader(r.RequestHeaders, "Session-Id", "Session_id")
			if sid == "" {
				sid = sessionField(turn, "session_id")
			}
			if sid != "" && "client:"+sid != child {
				parent = sid
			}
		}
	}
	if parent == "" {
		return "", fork
	}
	if normalized := hostConversationID(parent); normalized != "" {
		parent = normalized
	} else if cherryAgentSession.MatchString(parent) {
		parent = "client:pck:" + parent
	} else if strings.HasPrefix(child, "client:claude:") {
		parent = "client:claude:" + parent
	} else {
		parent = "client:" + parent
	}
	if parent == child {
		return "", fork
	}
	return parent, fork
}

func responseHistoryEnd(messages []Message, id string) int {
	end := 0
	for i, message := range messages {
		if id != "" && message.ResponseID == id {
			end = i + 1
		}
	}
	return end
}

func withoutHistoryNotice(input []Message) []Message {
	cleaned := make([]Message, 0, len(input))
	for _, message := range input {
		if message.Kind != "notice" {
			cleaned = append(cleaned, message)
		}
	}
	return cleaned
}

func branchMayInherit(previous, current *Record) bool {
	return previous.RequestID == current.RequestID && previous.ConversationTurns == 1 &&
		(previous.Branch == nil || previous.Branch.Status == "missing" || previous.Branch.Status == "linked")
}

func branchDescription(branch *ConversationBranch) string {
	switch branch.Status {
	case "response":
		return "已继承到父响应 " + branch.ResponseID + "；后续历史独立保存，统计不重复计入父请求。"
	case "context":
		return "已继承请求上下文确认的公共历史；后续历史独立保存，统计不重复计入父请求。"
	case "linked":
		return "已关联父会话；分叉位置尚未确认，仅展示请求提供的历史。"
	default:
		return "父会话或分叉点未被记录，仅展示请求提供的可用历史。"
	}
}

// Return only the prefix proven by a response anchor or by the full context
// the child actually resends. Parent IDs alone say nothing about the fork time.
func inheritedBranchHistory(parent *Record, input []Message, responseID string) ([]Message, string) {
	if parent == nil {
		return nil, "missing"
	}
	history := conversationMessages(parent)
	if responseID != "" {
		if end := responseHistoryEnd(history, responseID); end > 0 {
			return history[:end], "response"
		}
		return nil, "missing"
	}
	old, current := contextPoints(history), contextPoints(input)
	shared := 0
	for shared < len(old) && shared < len(current) && old[shared].digest == current[shared].digest {
		shared++
	}
	if shared > 0 {
		return history[:old[shared-1].end], "context"
	}
	return nil, "linked"
}

func inheritedResponseIDs(messages []Message) []string {
	seen := map[string]bool{}
	var ids []string
	for _, message := range messages {
		if message.ResponseID != "" && !seen[message.ResponseID] {
			ids = append(ids, message.ResponseID)
			seen[message.ResponseID] = true
		}
	}
	return ids
}

// Inherited originals keep their local archive references even when the
// parent's record is pruned. Copy only assets present in the child's history.
func copyBranchImages(tx *bolt.Tx, parentID, childID string, wanted map[string]bool) error {
	var parent, child []ImageAsset
	if raw := tx.Bucket(imageAssets).Get([]byte(parentID)); raw != nil {
		if err := json.Unmarshal(raw, &parent); err != nil {
			return err
		}
	}
	if raw := tx.Bucket(imageAssets).Get([]byte(childID)); raw != nil {
		if err := json.Unmarshal(raw, &child); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, asset := range child {
		seen[asset.ID] = true
	}
	for _, asset := range parent {
		if wanted[asset.ID] && !seen[asset.ID] && asset.Status == "ready" {
			child = append(child, asset)
			seen[asset.ID] = true
		}
	}
	if len(child) == 0 {
		return nil
	}
	raw, err := json.Marshal(child)
	if err != nil {
		return err
	}
	return tx.Bucket(imageAssets).Put([]byte(childID), raw)
}
