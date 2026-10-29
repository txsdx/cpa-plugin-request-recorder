package recorder

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

var cherryAgentSession = regexp.MustCompile(`^cherry-agent:[0-9a-f]{32}$`)
var claudeLegacySession = regexp.MustCompile(`_session_([0-9a-fA-F-]{36})$`)

func sessionValue(value string) string {
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		return ""
	}
	return value
}

func sessionHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := sessionValue(conversationHeader(headers, name)); value != "" {
			return value
		}
	}
	return ""
}

func conversationHeader(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func matchesConversationSession(r Summary, query string) bool {
	return r.SessionID == query || r.ClientSessionID == query || r.ClientSessionID == "client:"+query ||
		cherryAgentSession.MatchString(query) && r.ClientSessionID == "client:pck:"+query ||
		r.ClientSessionID != "" && r.ClientSessionID == hostConversationID(query)
}

func sessionField(body map[string]any, names ...string) string {
	for _, name := range names {
		if value := sessionValue(str(body[name])); value != "" {
			return value
		}
	}
	return ""
}

// Billing displays UsageRecord.SessionID verbatim. Only protocol identities
// that identify a conversation are useful here: cache, user, execution and
// derived routing identities do not in general identify a unique conversation.
func hostConversationID(value string) string {
	value = sessionValue(value)
	prefix, id, ok := strings.Cut(value, ":")
	if !ok || id == "" {
		return ""
	}
	switch prefix {
	case "codex", "header", "session", "thread", "conv", "agy", "task":
		return "client:" + id
	case "claude":
		return "client:" + value // Includes :agent:child, never the parent ID.
	case "pck":
		if cherryAgentSession.MatchString(id) {
			return "client:" + value
		}
	}
	return ""
}

func clientConversationID(r *Record) string {
	body := conversationBody(r)
	metadata := obj(body["metadata"])

	// Claude's JSON/legacy user_id embeds a session, rather than identifying a
	// user. A plain metadata.user_id is deliberately never used as a session.
	claudeID := sessionHeader(r.RequestHeaders, "X-Claude-Code-Session-Id")
	var claudeMeta map[string]any
	userID := str(metadata["user_id"])
	if strings.HasPrefix(strings.TrimSpace(userID), "{") {
		claudeMeta = decode(userID)
		if claudeID == "" {
			claudeID = sessionField(claudeMeta, "session_id")
		}
	} else if match := claudeLegacySession.FindStringSubmatch(userID); claudeID == "" && len(match) > 0 {
		claudeID = match[1]
	}
	if claudeID != "" {
		id := "client:claude:" + claudeID
		agent := sessionField(claudeMeta, "agent_id")
		if agent == "" {
			agent = sessionHeader(r.RequestHeaders, "X-Claude-Code-Agent-Id")
		}
		if agent == "" {
			agent = sessionField(metadata, "agent_id", "subagent_id")
		}
		if agent != "" && agent != "main" {
			id += ":agent:" + agent
		}
		return id
	}

	// Codex sends both underscore and hyphen spellings. Child threads and named
	// agents must not collapse into their parent's session.
	sid := sessionHeader(r.RequestHeaders, "Session-Id", "Session_id")
	tid := sessionHeader(r.RequestHeaders, "Thread-Id", "Thread_id")
	turn := codexTurnMetadata(r)
	if sid == "" {
		sid = sessionField(turn, "session_id")
	}
	if tid == "" {
		tid = sessionField(turn, "thread_id")
	}
	if tid == "" && sid != "" {
		tid = sessionField(body, "thread_id", "threadId")
		if tid == "" {
			tid = sessionField(metadata, "thread_id", "threadId")
		}
	}
	if sid != "" || tid != "" {
		// CPA resolves a fork before named-agent routing. Thread-Id can still
		// name the parent while Session-Id names the new fork.
		if parent := forkParentField(body, turn); parent != "" {
			child := tid
			if child == "" || child == parent {
				child = sid
			}
			if child == "" || child == parent {
				return ""
			}
			return "client:" + child
		}
		agent := sessionField(turn, "agent_name")
		agent = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(agent, "/root/"), "/"))
		if agent == "root" || agent == "main" {
			agent = ""
		}
		if agent != "" && sid != "" {
			return "client:" + sid + ":agent:" + agent
		}
		if tid != "" && tid != sid {
			return "client:" + tid
		}
		sub := sessionHeader(r.RequestHeaders, "X-Openai-Subagent")
		if sub != "" && !strings.EqualFold(sub, "false") && sub != "0" || str(turn["subagent_kind"]) == "thread_spawn" {
			return "" // Parent session alone cannot identify this child.
		}
		if sid == "" {
			return "client:" + tid
		}
		return "client:" + sid
	}
	if id := sessionHeader(r.RequestHeaders, "X-Session-ID", "X-Conversation-ID", "Conversation-ID", "X-Thread-ID", "X-Http-Session-Id"); id != "" {
		return "client:" + id
	}
	for _, candidate := range []map[string]any{body, metadata, obj(body["extra_body"])} {
		if id := sessionField(candidate, "conversation_id", "conversationId", "thread_id", "threadId", "chat_id", "chatId"); id != "" {
			if forkParentField(body, turn) == id {
				return ""
			}
			return "client:" + id
		}
		if id := sessionField(candidate, "child_session_id", "childSessionId", "session_id", "sessionId", "sessionID"); id != "" {
			if forkParentField(body, turn) == id {
				return ""
			}
			agent := sessionField(metadata, "agent_id", "subagent_id")
			if agent == "" {
				agent = sessionHeader(r.RequestHeaders, "X-Claude-Code-Agent-Id", "X-Agent-Id")
			}
			if agent != "" && agent != "main" && forkParentField(body, turn) == "" {
				return "client:" + id + ":agent:" + agent
			}
			return "client:" + id
		}
	}
	if id := sessionField(obj(body["conversation"]), "id"); id != "" {
		return "client:" + id
	}
	if id := sessionField(body, "conversation"); id != "" {
		return "client:" + id
	}
	// Cherry derives this exact format from SHA-256(agentSessionId), truncated
	// to 32 hex digits. Other prompt_cache_key values can be shared by chats.
	if id := sessionField(body, "prompt_cache_key", "promptCacheKey"); cherryAgentSession.MatchString(id) {
		return "client:pck:" + id
	}
	if id := hostConversationID(r.SessionID); id != "" {
		return id
	}
	return r.ClientSessionID
}
