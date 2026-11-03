package recorder

import "time"

// These are host observation times, not provider-generated message timestamps.
// Only the request's final speaker group is current input; earlier history may
// predate this recorder. Exact context merges retain the older stamped prefix.
func stampConversationTimes(input, output []Message, received, completed time.Time) {
	stamp := func(m *Message, at time.Time, source string) {
		if at.IsZero() {
			return
		}
		m.RecordedAt, m.TimeSource = &at, source
	}
	end := len(input) - 1
	for end >= 0 && (input[end].Role == "system" || input[end].Role == "developer" || input[end].Kind == "notice") {
		end--
	}
	if end >= 0 {
		role := input[end].Role
		for i := end; i >= 0 && input[i].Role == role && input[i].Kind != "notice"; i-- {
			stamp(&input[i], received, "request_received")
		}
	}
	for i := range output {
		stamp(&output[i], completed, "response_completed")
	}
}
