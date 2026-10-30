package recorder

import (
	"encoding/json"
	"sort"
	"strings"
)

// Tools can return JSON encoded inside a string, including image nodes without
// a type discriminator. Keep originals in the raw bodies and separate images
// from readable tool content before replay, matching, archiving, and export.
func normalizeToolImages(m *Message) {
	if m.Kind != "tool_result" || !mayContainToolImages(m.Content) {
		return
	}
	c := toolImageCollector{seen: map[string]bool{}}
	for _, img := range m.Images {
		c.seen[img.URL] = true
	}
	value := c.walk(m.Content, 0)
	if c.changes == 0 {
		return
	}
	if isRemovedToolImage(value) {
		m.Content = ""
	} else {
		m.Content = printable(value)
	}
	m.Images = append(m.Images, c.images...)
}

func mayContainToolImages(s string) bool {
	return strings.Contains(s, "data:image/") || strings.Contains(s, "image_url") ||
		strings.Contains(s, "b64_json") || strings.Contains(s, "inlineData") || strings.Contains(s, "inline_data") ||
		strings.Contains(s, "image_generation_call") || strings.Contains(s, "base64") && strings.Contains(s, "source") ||
		strings.Contains(s, "image/") && strings.Contains(s, "data")
}

type toolImageCollector struct {
	images  []Image
	seen    map[string]bool
	changes int
}

type removedToolImage struct{}

func isRemovedToolImage(v any) bool { _, removed := v.(removedToolImage); return removed }

func (c *toolImageCollector) take(node map[string]any) bool {
	images := imageMessage("tool", node).Images
	if len(images) == 0 {
		return false
	}
	for _, img := range images {
		if !c.seen[img.URL] && len(c.images) < 128 {
			c.images = append(c.images, img)
			c.seen[img.URL] = true
		}
	}
	c.changes++
	return true
}

func (c *toolImageCollector) walk(value any, depth int) any {
	if depth > 32 {
		return value
	}
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if strings.HasPrefix(trimmed, "data:image/") && c.take(map[string]any{"image_url": trimmed}) {
			return removedToolImage{}
		}
		if len(trimmed) == 0 || !mayContainToolImages(v) || trimmed[0] != '[' && trimmed[0] != '{' {
			return v
		}
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		var nested any
		if decoder.Decode(&nested) != nil || strings.TrimSpace(trimmed[decoder.InputOffset():]) != "" {
			return v
		}
		before := c.changes
		clean := c.walk(nested, depth+1)
		if c.changes == before {
			return v
		}
		if isRemovedToolImage(clean) {
			return clean
		}
		return printable(clean)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			clean := c.walk(item, depth+1)
			if !isRemovedToolImage(clean) {
				out = append(out, clean)
			}
		}
		if len(v) > 0 && len(out) == 0 {
			return removedToolImage{}
		}
		return out
	case map[string]any:
		removed := map[string]bool{}
		if v["image_url"] != nil && c.take(map[string]any{"image_url": v["image_url"]}) {
			removed["image_url"] = true
		}
		if v["b64_json"] != nil && c.take(map[string]any{"b64_json": v["b64_json"], "mime_type": v["mime_type"]}) {
			removed["b64_json"] = true
		}
		if source := obj(v["source"]); str(source["type"]) == "base64" && strings.HasPrefix(str(source["media_type"]), "image/") {
			if c.take(map[string]any{"source": source}) {
				removed["source"] = true
			}
		}
		for _, key := range []string{"inlineData", "inline_data"} {
			if source := obj(v[key]); strings.HasPrefix(str(source["mimeType"])+str(source["mime_type"]), "image/") {
				if c.take(map[string]any{key: source}) {
					removed[key] = true
				}
			}
		}
		typ := str(v["type"])
		if typ == "image_generation_call" && c.take(v) {
			removed["result"] = true
		}
		if typ == "image" || typ == "image_url" || typ == "input_image" || typ == "output_image" {
			if v["url"] != nil && c.take(map[string]any{"url": v["url"]}) {
				removed["url"] = true
			}
			if str(v["data"]) != "" && strings.HasPrefix(str(v["mimeType"])+str(v["mime_type"]), "image/") && c.take(v) {
				removed["data"] = true
			}
		}
		out := map[string]any{}
		useful := false
		hadImage := len(removed) > 0
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys) // stable image order is required for context matching
		for _, key := range keys {
			child := v[key]
			if removed[key] {
				continue
			}
			clean := c.walk(child, depth+1)
			if !isRemovedToolImage(clean) {
				out[key] = clean
				if key != "type" && key != "mime_type" && key != "mimeType" && key != "detail" && key != "width" && key != "height" {
					useful = true
				}
			} else {
				hadImage = true
			}
		}
		if hadImage && !useful {
			return removedToolImage{}
		}
		return out
	}
	return value
}
