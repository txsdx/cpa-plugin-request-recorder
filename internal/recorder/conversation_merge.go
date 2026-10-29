package recorder

func conversationConfiguration(messages []Message) ([]Message, []Message) {
	end := 0
	for end < len(messages) && (messages[end].Role == "system" || messages[end].Role == "developer") {
		end++
	}
	// Developer messages interleaved with tools/user turns are part of the
	// transcript, not global instructions to collect and move to its beginning.
	return messages[:end], messages[end:]
}

// Locate an exact incoming context prefix in the archived conversation. This
// handles a client resending only a context tail after trimming/compaction.
// KMP keeps long tool conversations linear in message count. Matching an
// isolated user prompt is insufficient: "continue" is often repeated.
func conversationOverlap(previous, input []Message) (oldEnd, newEnd int) {
	points := func(messages []Message) []contextPoint {
		var result []contextPoint
		for i, m := range messages {
			if m.Kind != "reasoning" && m.Kind != "notice" && m.Choice == 0 && m.Role != "system" && m.Role != "developer" {
				result = append(result, contextPoint{contextMessageID(m), i + 1, m.Role})
			}
		}
		return result
	}
	pattern := points(input)
	if len(pattern) == 0 {
		return 0, 0
	}
	prefix := make([]int, len(pattern))
	progress := make([]bool, len(pattern))
	for i := range pattern {
		progress[i] = pattern[i].role == "assistant" || pattern[i].role == "tool" || i > 0 && progress[i-1]
		if i == 0 {
			continue
		}
		j := prefix[i-1]
		for j > 0 && pattern[i].digest != pattern[j].digest {
			j = prefix[j-1]
		}
		if pattern[i].digest == pattern[j].digest {
			j++
		}
		prefix[i] = j
	}
	matched, best := 0, 0
	for _, point := range points(previous) {
		for matched > 0 && (matched == len(pattern) || point.digest != pattern[matched].digest) {
			matched = prefix[matched-1]
		}
		if point.digest == pattern[matched].digest {
			matched++
		}
		if matched > 0 && matched >= best && progress[matched-1] {
			best, oldEnd, newEnd = matched, point.end, pattern[matched-1].end
		}
	}
	return oldEnd, newEnd
}

func mergeConversation(previous, input, output []Message, explicit bool) []Message {
	oldConfig, previous := conversationConfiguration(previous)
	config, input := conversationConfiguration(input)
	if len(config) == 0 {
		config = oldConfig
	}
	oldPoints, newPoints := contextPoints(previous), contextPoints(input)
	shared := 0
	for shared < len(oldPoints) && shared < len(newPoints) && oldPoints[shared].digest == newPoints[shared].digest {
		shared++
	}
	if explicit {
		onlyUsers := len(newPoints) > 0
		for _, point := range newPoints {
			onlyUsers = onlyUsers && point.role == "user"
		}
		if onlyUsers {
			shared = 0 // An incremental repeated question is not proof of a rewind.
		}
	}
	merged := append([]Message{}, config...)
	if shared > 0 {
		merged = append(merged, previous[:oldPoints[shared-1].end]...)
		merged = append(merged, input[newPoints[shared-1].end:]...)
	} else if explicit {
		oldEnd, newEnd := conversationOverlap(previous, input)
		if oldEnd == 0 {
			oldEnd = len(previous) // Incremental input without a repeated tail.
		}
		merged = append(merged, previous[:oldEnd]...)
		merged = append(merged, input[newEnd:]...)
	} else {
		merged = append(merged, input...)
	}
	return append(merged, output...)
}
