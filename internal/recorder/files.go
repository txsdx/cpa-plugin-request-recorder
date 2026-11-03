package recorder

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// URL holds the original data only in stored history. decorateFiles removes it
// before sending chat data to a browser; downloads use authenticated archive IDs.
type File struct {
	ID        string `json:"id,omitempty"`
	Filename  string `json:"filename"`
	URL       string `json:"url,omitempty"`
	Reference string `json:"reference,omitempty"`
	MIME      string `json:"mime,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

func safeFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return "attachment"
	}
	// Bound UTF-8 bytes without cutting a code point; keep the extension.
	ext := path.Ext(name)
	if len(ext) > 24 {
		ext = ""
	}
	if len(name) > 200 {
		base := strings.TrimSuffix(name, ext)
		var trimmed strings.Builder
		for _, r := range base {
			if trimmed.Len()+len(string(r)) > 200-len(ext) {
				break
			}
			trimmed.WriteRune(r)
		}
		name = trimmed.String() + ext
	}
	return name
}

func fileMessage(role string, block map[string]any) Message {
	b := block
	if nested := obj(block["file"]); nested != nil {
		b = nested
	}
	f := File{Filename: str(b["filename"]), MIME: str(b["mime_type"])}
	if f.Filename == "" {
		f.Filename = str(b["name"])
	}
	if f.Filename == "" {
		f.Filename = str(b["title"])
	}
	if f.MIME == "" {
		f.MIME = str(b["media_type"])
	}
	f.URL, f.Reference = str(b["file_url"]), str(b["file_id"])
	if data, ok := b["file_data"].(string); ok {
		f.URL = data
		if !strings.HasPrefix(data, "data:") {
			f.URL = "data:" + fileMediaType(f.MIME) + ";base64," + data
		}
	}
	if src := obj(b["source"]); src != nil {
		f.MIME = str(src["media_type"])
		switch str(src["type"]) {
		case "base64":
			f.URL = "data:" + fileMediaType(f.MIME) + ";base64," + str(src["data"])
		case "text":
			f.URL = "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(str(src["data"])))
		case "url":
			f.URL = str(src["url"])
		case "file":
			f.Reference = str(src["file_id"])
		}
	}
	for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
		if src := obj(b[key]); src != nil {
			f.MIME = str(src["mimeType"])
			if f.MIME == "" {
				f.MIME = str(src["mime_type"])
			}
			if f.Filename == "" {
				f.Filename = str(src["displayName"])
			}
			if f.Filename == "" {
				f.Filename = str(src["display_name"])
			}
			if data, ok := src["data"].(string); ok {
				f.URL = "data:" + fileMediaType(f.MIME) + ";base64," + data
			}
			if uri := str(src["fileUri"]); uri != "" {
				f.URL = uri
			}
			if uri := str(src["file_uri"]); uri != "" {
				f.URL = uri
			}
		}
	}
	if f.Filename == "" && !strings.HasPrefix(f.URL, "data:") {
		if u, err := url.Parse(f.URL); err == nil && u.Scheme == "https" {
			f.Filename, _ = url.PathUnescape(path.Base(u.Path))
		}
	}
	if f.Filename == "" {
		if meta, _, ok := strings.Cut(f.URL, ","); ok && strings.HasPrefix(meta, "data:") && f.MIME == "" {
			f.MIME = fileMediaType(strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64"))
		}
		ext := map[string]string{"application/pdf": ".pdf", "text/plain": ".txt", "text/csv": ".csv", "application/json": ".json", "application/zip": ".zip"}[fileMediaType(f.MIME)]
		f.Filename = "attachment" + ext
	}
	f.Filename = safeFileName(f.Filename)
	if f.URL == "" {
		f.Status = "unavailable"
		f.Error = "请求未包含原文件内容或可下载链接，无法获取原文件"
	}
	return Message{Role: role, Kind: "attachment", Files: []File{f}}
}

// Old attachment blocks are normalized on access, without a database-wide scan.
func normalizeMessageFiles(m *Message) {
	if m.Kind != "attachment" || len(m.Files) > 0 {
		return
	}
	var block map[string]any
	if json.Unmarshal([]byte(m.Content), &block) != nil {
		return
	}
	typ := str(block["type"])
	if typ != "file" && typ != "document" && typ != "input_file" && block["inlineData"] == nil && block["inline_data"] == nil && block["fileData"] == nil && block["file_data"] == nil {
		return
	}
	parsed := fileMessage(m.Role, block)
	m.Files, m.Content = parsed.Files, ""
}

func fileID(f File) string { return fingerprint("file\x00" + f.Filename + "\x00" + f.URL) }
func recordFileSources(r *Record, limit int) []imageSource {
	var sources []imageSource
	seen := map[string]bool{}
	for _, m := range conversationMessages(r) {
		for _, f := range m.Files {
			if f.URL == "" {
				continue
			}
			id := fileID(f)
			if seen[id] {
				continue
			}
			seen[id] = true
			sources = append(sources, imageSource{id: id, url: f.URL, name: f.Filename})
			if len(sources) >= limit {
				return sources
			}
		}
	}
	return sources
}
func fileDirectory(cfg Config) string {
	if cfg.FilesDir != "" {
		return cfg.FilesDir
	}
	base := filepath.Base(cfg.DBPath)
	return filepath.Join(filepath.Dir(cfg.DBPath), strings.TrimSuffix(base, filepath.Ext(base))+"-files")
}
func (a *assetArchive) cacheName(name string) bool {
	if a.fileMode {
		return cacheFileName.MatchString(name)
	}
	return cacheImageName.MatchString(name)
}
func (a *assetArchive) fileName(asset ImageAsset) string {
	if !a.fileMode {
		return imageFileName(asset)
	}
	if !validSHA256(asset.Hash) {
		return ""
	}
	return asset.Hash + ".bin"
}
func (a *assetArchive) payloadMIME(data []byte) (string, error) {
	if a.fileMode {
		return fileMediaType(http.DetectContentType(data)), nil
	}
	return imageMIME(data)
}
func (a *assetArchive) decorateFiles(messages []Message, inventory []ImageAsset) bool {
	byID := map[string]ImageAsset{}
	for _, asset := range inventory {
		byID[asset.ID] = asset
	}
	pending := false
	for mi := range messages {
		for fi := range messages[mi].Files {
			f := &messages[mi].Files[fi]
			if f.URL == "" {
				continue
			}
			f.ID = fileID(*f)
			f.URL = ""
			if asset, ok := byID[f.ID]; ok {
				f.Status, f.Error, f.MIME, f.Size = asset.Status, asset.Error, asset.MIME, asset.Size
				if asset.Status == "ready" && !a.exists(asset) {
					f.Status, f.Error = "pending", ""
				}
			} else if len(inventory) >= a.cfg.MaxImagesPerRecord {
				f.Status, f.Error = "blocked", "超过单条记录文件数量限制"
			} else {
				f.Status, f.Error = "pending", ""
			}
			pending = pending || f.Status == "pending"
		}
	}
	return pending
}
func fileMediaType(value string) string {
	typ, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(typ, "/") {
		return "application/octet-stream"
	}
	return typ
}
func inlineFile(raw string, max int64) ([]byte, string, error) {
	meta, payload, ok := strings.Cut(raw, ",")
	if !ok || !strings.HasPrefix(meta, "data:") {
		return nil, "", errors.New("内嵌文件格式无效")
	}
	var data []byte
	var err error
	if strings.HasSuffix(strings.ToLower(meta), ";base64") {
		if int64(len(payload)) > ((max+2)/3)*4+4 {
			return nil, "", errors.New("内嵌文件超过大小限制")
		}
		data, err = base64.StdEncoding.DecodeString(payload)
	} else {
		if int64(len(payload)) > max*3 {
			return nil, "", errors.New("内嵌文件超过大小限制")
		}
		var text string
		text, err = url.PathUnescape(payload)
		data = []byte(text)
	}
	if err != nil {
		return nil, "", errors.New("内嵌文件编码无效")
	}
	if int64(len(data)) > max {
		return nil, "", errors.New("文件超过单个文件大小限制")
	}
	typ := fileMediaType(strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64"))
	if typ == "application/octet-stream" {
		typ = fileMediaType(http.DetectContentType(data))
	}
	return data, typ, nil
}
