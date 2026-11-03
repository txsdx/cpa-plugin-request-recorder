package recorder

import (
	"testing"
	"time"
)

func TestConversationTimesPreserveObservedHistoryAcrossToolTurns(t *testing.T) {
	s := conversationStore(t)
	user := chat("user", "current question")
	oldUser, oldModel := chat("user", "imported old question"), chat("assistant", "imported old answer")
	tool := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_time", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}}
	first := conversationTurn("time1", "key", "time-session", chatRequest(oldUser, oldModel, user), "", 1)
	first.ResponseBody = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_time","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`
	check(t, s.Save(first))
	initial := conversationMessages(first)
	if initial[0].RecordedAt != nil || initial[1].RecordedAt != nil {
		t.Fatal("invented times for imported history")
	}
	if initial[2].RecordedAt == nil || !initial[2].RecordedAt.Equal(first.StartedAt) || initial[2].TimeSource != "request_received" {
		t.Fatal(initial[2])
	}
	if initial[3].RecordedAt == nil || !initial[3].RecordedAt.Equal(first.CompletedAt) || initial[3].TimeSource != "response_completed" {
		t.Fatal(initial[3])
	}
	result := map[string]any{"role": "tool", "tool_call_id": "call_time", "content": "tool finished"}
	second := conversationTurn("time2", "key", "time-session", chatRequest(oldUser, oldModel, user, tool, result), chatReply("final answer"), 3)
	check(t, s.Save(second))
	if second.ID != first.ID {
		t.Fatal("timestamps changed context identity")
	}
	all := conversationMessages(second)
	if len(all) != 6 {
		t.Fatal(all)
	}
	if !all[2].RecordedAt.Equal(first.StartedAt) || !all[3].RecordedAt.Equal(first.CompletedAt) {
		t.Fatal("old times replaced by latest turn time")
	}
	if all[4].TimeSource != "request_received" || !all[4].RecordedAt.Equal(second.StartedAt) {
		t.Fatal("tool result was not stamped on receipt", all[4])
	}
	if all[5].TimeSource != "response_completed" || !all[5].RecordedAt.Equal(second.CompletedAt) {
		t.Fatal("final model time missing", all[5])
	}
	before := second.ConversationHistory
	check(t, s.Save(second))
	if before != second.ConversationHistory {
		t.Fatal("metadata save restamped history")
	}
}

func TestConversationTimeDoesNotChangeContextAndZeroTimesStayUnknown(t *testing.T) {
	input := []Message{message("user", "text", "question")}
	output := []Message{message("assistant", "text", "answer")}
	id := contextMessageID(input[0])
	stampConversationTimes(input, output, time.Time{}, time.Time{})
	if input[0].RecordedAt != nil || output[0].RecordedAt != nil {
		t.Fatal("zero time was invented")
	}
	stampConversationTimes(input, output, time.Now(), time.Now())
	if id != contextMessageID(input[0]) {
		t.Fatal("observation time affected context identity")
	}
}
