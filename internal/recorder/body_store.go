package recorder

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	bolt "go.etcd.io/bbolt"
)

var bodyChunks = []byte("body_chunks_v1")
var bodyCounts = []byte("body_chunk_refs_v1")

const maxChunkSize = 64 << 10

type bodyManifest struct {
	Size   int      `json:"size"`
	Chunks []string `json:"chunks"`
}
type storedRecord struct {
	Record
	StorageVersion int                     `json:"_storage_version,omitempty"`
	Bodies         map[string]bodyManifest `json:"_bodies,omitempty"`
}

func recordBodyFields(r *Record) map[string]*string {
	return map[string]*string{"conversation_history": &r.ConversationHistory, "request_body": &r.RequestBody, "intercepted_body": &r.InterceptedBody, "response_body": &r.ResponseBody, "upstream_requests": &r.UpstreamRequests, "upstream_responses": &r.UpstreamResponses}
}

// A content-defined boundary realigns after insertions. Shared conversation
// history and base64 payloads deduplicate even when JSON prefixes/offsets differ.
// Boundary hashes do not identify data; every stored chunk uses SHA-256.
var chunkGear = func() [256]uint64 {
	var table [256]uint64
	for i := range table {
		v := uint64(i) + 0x9e3779b97f4a7c15
		v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
		v = (v ^ (v >> 27)) * 0x94d049bb133111eb
		table[i] = v ^ (v >> 31)
	}
	return table
}()

func splitBody(body string, visit func(string)) {
	start := 0
	var rolling uint64
	for i := 0; i < len(body); i++ {
		rolling = rolling<<1 + chunkGear[body[i]]
		size := i + 1 - start
		if size >= maxChunkSize || size >= 4096 && rolling&0x3fff == 0 {
			visit(body[start : i+1])
			start, rolling = i+1, 0
		}
	}
	if start < len(body) {
		visit(body[start:])
	}
}

var bodyGzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// Each chunk is at most 64 KiB. Use raw bytes when compression would expand it.
// Five-byte header: codec (0/raw, 1/gzip) and decoded length (big endian).
func encodeBodyChunk(body string) ([]byte, error) {
	var buf bytes.Buffer
	w := bodyGzipWriters.Get().(*gzip.Writer)
	w.Reset(&buf)
	_, err := io.WriteString(w, body)
	if err == nil {
		err = w.Close()
	}
	w.Reset(io.Discard)
	bodyGzipWriters.Put(w)
	if err != nil {
		return nil, err
	}
	codec, payload := byte(1), buf.Bytes()
	if len(payload) >= len(body) {
		codec, payload = 0, []byte(body)
	}
	out := make([]byte, 5+len(payload))
	out[0] = codec
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	copy(out[5:], payload)
	return out, nil
}
func decodeBodyChunk(id string, data []byte) (string, error) {
	if len(data) < 5 {
		return "", errors.New("truncated body chunk")
	}
	size := int(binary.BigEndian.Uint32(data[1:5]))
	if size < 1 || size > maxChunkSize {
		return "", errors.New("invalid body chunk size")
	}
	body := data[5:]
	if data[0] == 1 {
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return "", errors.New("invalid compressed body chunk")
		}
		body, err = io.ReadAll(io.LimitReader(r, int64(size)+1))
		r.Close()
		if err != nil {
			return "", errors.New("body chunk decompression failed")
		}
	} else if data[0] != 0 {
		return "", errors.New("unsupported body chunk codec")
	}
	sum := sha256.Sum256(body)
	if len(body) != size || hex.EncodeToString(sum[:]) != id {
		return "", errors.New("body chunk integrity check failed")
	}
	return string(body), nil
}

func prepareStoredRecord(r *Record) (storedRecord, map[string]string) {
	stored := storedRecord{Record: *r, StorageVersion: 1, Bodies: map[string]bodyManifest{}}
	stored.Images, stored.KeyAlias = nil, ""
	chunks := map[string]string{}
	for name, field := range recordBodyFields(&stored.Record) {
		if *field == "" {
			continue
		}
		manifest := bodyManifest{Size: len(*field)}
		splitBody(*field, func(body string) {
			sum := sha256.Sum256([]byte(body))
			id := hex.EncodeToString(sum[:])
			manifest.Chunks = append(manifest.Chunks, id)
			chunks[id] = body
		})
		stored.Bodies[name] = manifest
		*field = ""
	}
	return stored, chunks
}
func storedChunkIDs(raw []byte) (map[string]bool, error) {
	var stored struct {
		StorageVersion int                     `json:"_storage_version"`
		Bodies         map[string]bodyManifest `json:"_bodies"`
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &stored); err != nil {
			return nil, err
		}
	}
	if stored.StorageVersion != 1 && raw != nil {
		return nil, errors.New("unsupported record storage version")
	}
	ids := map[string]bool{}
	for _, body := range stored.Bodies {
		for _, id := range body.Chunks {
			ids[id] = true
		}
	}
	return ids, nil
}
func decodeStoredRecord(tx *bolt.Tx, raw []byte) (*Record, error) {
	var stored storedRecord
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	if stored.StorageVersion != 1 {
		return nil, errors.New("unsupported record storage version")
	}
	fields := recordBodyFields(&stored.Record)
	for name, manifest := range stored.Bodies {
		field, ok := fields[name]
		if !ok || manifest.Size < 1 || len(manifest.Chunks) == 0 || manifest.Size < len(manifest.Chunks) || manifest.Size/maxChunkSize > len(manifest.Chunks) {
			return nil, errors.New("invalid body manifest")
		}
		var body strings.Builder
		for _, id := range manifest.Chunks {
			if !validSHA256(id) {
				return nil, errors.New("invalid body reference")
			}
			chunk, err := decodeBodyChunk(id, tx.Bucket(bodyChunks).Get([]byte(id)))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if len(chunk) > manifest.Size-body.Len() {
				return nil, errors.New("body manifest length mismatch")
			}
			body.WriteString(chunk)
		}
		if body.Len() != manifest.Size {
			return nil, errors.New("body manifest length mismatch")
		}
		*field = body.String()
	}
	return &stored.Record, nil
}

func releaseBodyChunk(tx *bolt.Tx, id string) error {
	key := []byte(id)
	count := tx.Bucket(bodyCounts).Get(key)
	if len(count) != 8 || binary.BigEndian.Uint64(count) < 1 {
		return errors.New("invalid body reference count")
	}
	refs := binary.BigEndian.Uint64(count) - 1
	if refs == 0 {
		if err := tx.Bucket(bodyChunks).Delete(key); err != nil {
			return err
		}
		return tx.Bucket(bodyCounts).Delete(key)
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], refs)
	return tx.Bucket(bodyCounts).Put(key, encoded[:])
}
