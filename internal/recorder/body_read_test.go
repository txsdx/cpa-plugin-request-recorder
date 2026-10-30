package recorder

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestPooledBodyReadersRejectCorruptionAndReset(t *testing.T) {
	body := strings.Repeat("完整文字 compressed body ", 1000)
	sum := sha256.Sum256([]byte(body))
	id := hex.EncodeToString(sum[:])
	encoded, err := encodeBodyChunk(body)
	check(t, err)
	if encoded[0] != 1 {
		t.Fatal("fixture must be compressed")
	}
	for _, corrupt := range []func([]byte){
		func(b []byte) { b[len(b)-8] ^= 1 }, // invalid gzip CRC
		func(b []byte) { binary.BigEndian.PutUint32(b[1:5], uint32(len(body)-1)) },
		func(b []byte) { binary.BigEndian.PutUint32(b[1:5], uint32(len(body)+1)) },
		func(b []byte) { b[5] = 0 }, // invalid gzip header
	} {
		bad := append([]byte(nil), encoded...)
		corrupt(bad)
		if _, err := decodeBodyChunk(id, bad); err == nil {
			t.Fatal("accepted corrupt chunk")
		}
		got, err := decodeBodyChunk(id, encoded)
		check(t, err)
		if got != body {
			t.Fatal("reader retained a prior chunk's state")
		}
	}
	for _, missing := range []int{1, 4, 8} {
		if _, err := decodeBodyChunk(id, encoded[:len(encoded)-missing]); err == nil {
			t.Fatal("accepted a truncated gzip trailer despite matching decoded bytes")
		}
	}
}

func TestConversationReadDoesNotDecompressUnusedOriginals(t *testing.T) {
	s := conversationStore(t)
	request, _ := json.Marshal(map[string]any{"input": "question", "unused_metadata": strings.Repeat("not part of the conversation ", 10000)})
	r := conversationTurn("r", "key", "session", string(request), chatReply("answer"), 1)
	check(t, s.Save(r))
	check(t, s.update(0, func(tx *bolt.Tx) error {
		var stored storedRecord
		if err := json.Unmarshal(tx.Bucket(records).Get([]byte(r.ID)), &stored); err != nil {
			return err
		}
		history := map[string]bool{}
		for _, id := range stored.Bodies["conversation_history"].Chunks {
			history[id] = true
		}
		for _, id := range stored.Bodies["request_body"].Chunks {
			if !history[id] {
				return tx.Bucket(bodyChunks).Put([]byte(id), []byte("damaged original chunk"))
			}
		}
		t.Fatal("fixture has no request-only chunk")
		return nil
	}))
	chat, err := s.getConversation(r.ID)
	check(t, err)
	if chat.RequestBody != "" || chat.ResponseBody != "" || chat.ConversationHistory != r.ConversationHistory {
		t.Fatal("conversation-only read changed history or loaded hidden originals")
	}
	if _, err := s.Get(r.ID); err == nil {
		t.Fatal("full read must still verify original chunk integrity")
	}
}
