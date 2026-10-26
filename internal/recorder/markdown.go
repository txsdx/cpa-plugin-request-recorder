package recorder

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

var errMarkdownTooLarge = errors.New("Markdown 导出超过 max_body_bytes 限制，请增大配置后重试")

type markdownDocument struct {
	body, images strings.Builder
	references   map[string]string
	bytes, limit int
}

func (d *markdownDocument) write(to *strings.Builder, text string) error {
	if len(text) > d.limit-d.bytes {
		return errMarkdownTooLarge
	}
	d.bytes += len(text)
	_, err := to.WriteString(text)
	return err
}
func markdownLabel(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#").Replace(text)
}
func markdownFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	language := "text"
	if json.Valid([]byte(text)) {
		language = "json"
	}
	return fence + language + "\n" + text + "\n" + fence + "\n\n"
}

func (e *Engine) markdownImage(r *Record, image Image, d *markdownDocument) (string, error) {
	id := imageID(image.URL)
	if ref := d.references[id]; ref != "" {
		return "![原图][" + ref + "]\n\n", nil
	}
	var data []byte
	var mime string
	var imageErr error
	for _, asset := range r.Images {
		if asset.ID != id {
			continue
		}
		if asset.Status == "ready" {
			data, imageErr = e.images.read(asset)
			mime = asset.MIME
		} else {
			imageErr = errors.New(asset.Error)
		}
		break
	}
	if errors.Is(imageErr, errImageBusy) {
		return "", imageErr
	}
	if len(data) == 0 && strings.HasPrefix(image.URL, "data:") {
		data, mime, imageErr = inlineImage(image.URL, e.cfg.MaxImageBytes)
	}
	if len(data) == 0 {
		reason := "原图尚未归档或不可用"
		if imageErr != nil && imageErr.Error() != "" {
			reason = imageErr.Error()
		}
		return "图片未能导出：" + markdownLabel(reason) + "。\n\n", nil
	}
	ref := fmt.Sprintf("image-%d", len(d.references)+1)
	prefix := "[" + ref + "]: data:" + mime + ";base64,"
	encoded := base64.StdEncoding.EncodedLen(len(data))
	if encoded+len(prefix)+2 > d.limit-d.bytes {
		return "", errMarkdownTooLarge
	}
	if err := d.write(&d.images, prefix); err != nil {
		return "", err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, &d.images)
	if _, err := encoder.Write(data); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	d.bytes += encoded
	if err := d.write(&d.images, "\n\n"); err != nil {
		return "", err
	}
	d.references[id] = ref
	return "![原图][" + ref + "]\n\n", nil
}

func (e *Engine) renderMarkdown(r *Record) ([]byte, error) {
	d := &markdownDocument{references: map[string]string{}, limit: e.cfg.MaxBodyBytes}
	write := func(text string) error { return d.write(&d.body, text) }
	model := r.ResponseModel
	if model == "" {
		model = r.Model
	}
	if model == "" {
		model = r.RequestedModel
	}
	if err := write("# 完整会话记录\n\n- 会话 ID：" + markdownLabel(r.ID) + "\n- 模型：" + markdownLabel(model) + "\n"); err != nil {
		return nil, err
	}
	if r.KeyAlias != "" {
		if err := write("- 请求者：" + markdownLabel(r.KeyAlias) + "\n"); err != nil {
			return nil, err
		}
	}
	if !r.StartedAt.IsZero() {
		if err := write("- 最新请求时间：" + r.StartedAt.Format(time.RFC3339) + "\n"); err != nil {
			return nil, err
		}
	}
	if err := write("\n"); err != nil {
		return nil, err
	}
	for _, warning := range r.Warnings {
		if err := write("> 采集提示：" + markdownLabel(warning) + "\n\n"); err != nil {
			return nil, err
		}
	}
	if r.Error != "" {
		if err := write("> 请求错误：" + markdownLabel(r.Error) + "\n\n"); err != nil {
			return nil, err
		}
	}
	roles := map[string]string{"system": "系统", "developer": "开发者", "user": "用户", "assistant": "模型", "tool": "工具"}
	kinds := map[string]string{"reasoning": "思考 / 摘要", "tool_call": "工具调用", "tool_result": "工具返回", "attachment": "附件", "image": "图片", "notice": "上下文提示", "error": "错误"}
	for i, message := range conversationMessages(r) {
		role := roles[message.Role]
		if role == "" {
			role = message.Role
		}
		title := fmt.Sprintf("## %d. %s", i+1, markdownLabel(role))
		if kind := kinds[message.Kind]; kind != "" {
			title += " · " + kind
		}
		if message.Choice > 0 {
			title += fmt.Sprintf(" · 候选 %d", message.Choice)
		}
		if err := write(title + "\n\n"); err != nil {
			return nil, err
		}
		if message.Name != "" {
			if err := write("工具：" + markdownLabel(message.Name) + "\n\n"); err != nil {
				return nil, err
			}
		}
		if message.ToolCallID != "" {
			if err := write("调用 ID：" + markdownLabel(message.ToolCallID) + "\n\n"); err != nil {
				return nil, err
			}
		}
		if message.Content != "" {
			content := message.Content + "\n\n"
			if message.Kind != "text" {
				content = markdownFence(message.Content)
			}
			if err := write(content); err != nil {
				return nil, err
			}
		}
		for _, image := range message.Images {
			content, err := e.markdownImage(r, image, d)
			if err != nil {
				return nil, err
			}
			if err = write(content); err != nil {
				return nil, err
			}
		}
	}
	return []byte(d.body.String() + d.images.String()), nil
}

func (e *Engine) markdownResponse(id string) wire.ManagementResponse {
	r, err := e.Get(id)
	if errors.Is(err, ErrNotFound) {
		return result(404, map[string]string{"error": "record not found"})
	}
	if err != nil {
		return result(500, map[string]string{"error": "database read failed"})
	}
	body, err := e.renderMarkdown(r)
	if errors.Is(err, errMarkdownTooLarge) {
		return result(413, map[string]string{"error": err.Error()})
	}
	if errors.Is(err, errImageBusy) {
		return result(429, map[string]string{"error": "图片正在读取，请稍后重新导出"})
	}
	if err != nil {
		return result(500, map[string]string{"error": "Markdown export failed"})
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, r.ID)
	return wire.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/markdown; charset=utf-8"}, "Content-Disposition": {"attachment; filename=\"conversation-" + name + ".md\""}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; sandbox"}}, Body: body}
}
