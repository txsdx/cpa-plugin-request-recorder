package recorder

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

const PluginID = "request-recorder"
const APIBase = "/v0/management/plugins/" + PluginID
const ResourceBase = "/v0/resource/plugins/" + PluginID

// Version is injected from the validated release tag by the build.
var Version = "0.6.1"

func Registration() map[string]any {
	return map[string]any{"schema_version": 6, "metadata": map[string]any{
		"Name": PluginID, "Version": Version, "Author": "txsdx", "GitHubRepository": "https://github.com/txsdx/cpa-plugin-request-recorder",
		"ConfigFields": []map[string]string{
			{"Name": "max_database_bytes", "Type": "integer", "Description": "数据库容量上限，默认 1 GiB；自动淘汰最旧记录并整理，图片和 CPA 源日志另计"},
			{"Name": "refresh_interval_seconds", "Type": "integer", "Description": "列表自动刷新间隔秒数，默认 1 秒，范围 1..3600"},
			{"Name": "db_path", "Type": "string", "Description": "持久化数据库路径，变更需重启"},
			{"Name": "images_dir", "Type": "string", "Description": "图片归档目录，默认数据库旁的 <数据库名>-images"},
			{"Name": "preview_quality", "Type": "integer", "Description": "在线 WebP 预览画质，默认 75，范围 1..100；原图下载不改变"},
			{"Name": "preview_max_dimension", "Type": "integer", "Description": "WebP 预览最长边，默认 1600 像素，范围 128..4096；小图不放大"},
			{"Name": "archive_remote_images", "Type": "boolean", "Description": "由服务器下载并保存公网 HTTPS 图片"},
			{"Name": "max_image_files", "Type": "integer", "Description": "图片缓存最大文件数（防止大量小图耗尽 inode）"},
			{"Name": "max_image_bytes", "Type": "integer", "Description": "单张图片最大字节数"},
			{"Name": "max_image_storage_bytes", "Type": "integer", "Description": "图片归档总容量上限"},
			{"Name": "max_images_per_record", "Type": "integer", "Description": "每条记录最多归档图片数"},
			{"Name": "image_workers", "Type": "integer", "Description": "后台图片下载并发数"},
			{"Name": "image_download_timeout_seconds", "Type": "integer", "Description": "单张图片下载超时秒数"},
			{"Name": "retention_days", "Type": "integer", "Description": "保留天数，0 不按时间清理"},
			{"Name": "max_records", "Type": "integer", "Description": "最多条目数，0 不限制"},
			{"Name": "max_body_bytes", "Type": "integer", "Description": "单份请求/响应最大采集字节数，超限明确标记"},
			{"Name": "max_inflight", "Type": "integer", "Description": "最大并发采集请求数"},
			{"Name": "max_memory_bytes", "Type": "integer", "Description": "正在采集的正文内存总预算"},
		},
	}, "capabilities": map[string]bool{"request_interceptor": true, "request_lifecycle_plugin": true, "response_interceptor": true, "response_stream_interceptor": true, "usage_plugin": true, "management_api": true}}
}
func ManagementRegistration() map[string]any {
	routes := []map[string]string{}
	for _, p := range [][2]string{{"GET", "/records"}, {"GET", "/record"}, {"GET", "/export/markdown"}, {"GET", "/status"}, {"GET", "/storage"}, {"GET", "/image"}, {"GET", "/images"}, {"POST", "/settings"}, {"POST", "/aliases"}, {"POST", "/aliases/sync"}, {"POST", "/images/retry"}} {
		routes = append(routes, map[string]string{"Method": p[0], "Path": APIBase + p[1]})
	}
	return map[string]any{"Routes": routes, "Resources": []map[string]string{
		{"Path": "/index.html", "Menu": "请求记录", "Description": "请求原文、上游响应与对话回放"},
		{"Path": "/app.js"}, {"Path": "/style.css"},
	}}
}
func result(code int, v any) wire.ManagementResponse {
	b, err := json.Marshal(v)
	if err != nil {
		code = 500
		b = []byte(`{"error":"encode failed"}`)
	}
	return wire.ManagementResponse{StatusCode: code, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, Body: b}
}

// Management data endpoints must be mounted ONLY under the host's authenticated
// management prefix. Public resource routes contain static assets, never data.
func (e *Engine) Management(req wire.ManagementRequest, web fs.FS) wire.ManagementResponse {
	if strings.HasPrefix(req.Path, ResourceBase+"/") {
		if req.Method != "GET" {
			return result(405, map[string]string{"error": "method not allowed"})
		}
		name := strings.TrimPrefix(req.Path, ResourceBase+"/")
		mime := map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "style.css": "text/css; charset=utf-8"}[name]
		if mime == "" {
			return result(404, map[string]string{"error": "not found"})
		}
		b, err := fs.ReadFile(web, name)
		if err != nil {
			return result(500, map[string]string{"error": "asset unavailable"})
		}
		return wire.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {mime}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'self'; script-src 'self'; style-src 'self'; img-src blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"}, "Referrer-Policy": {"no-referrer"}}, Body: b}
	}
	if !strings.HasPrefix(req.Path, APIBase+"/") {
		return result(404, map[string]string{"error": "not found"})
	}
	path := strings.TrimPrefix(req.Path, APIBase)
	if req.Method == "GET" {
		switch path {
		case "/export/markdown":
			return e.markdownResponse(req.Query.Get("id"))
		case "/storage":
			stats, err := e.store.StorageStats()
			if err != nil {
				return result(500, map[string]string{"error": "storage diagnostics failed"})
			}
			return result(200, stats)
		case "/status":
			return result(200, e.Status())
		case "/records":
			q := req.Query
			f := Filter{Query: q.Get("q"), KeyID: q.Get("key_id"), Alias: q.Get("alias"), Model: q.Get("model"), Provider: q.Get("provider"), Outcome: q.Get("outcome"), Session: q.Get("session"), Reasoning: q.Get("reasoning"), IgnoreRejected: e.store.Settings(e.cfg.RefreshIntervalSeconds).IgnoreRejected}
			var err error
			if q.Has("ignore_rejected") {
				f.IgnoreRejected, err = strconv.ParseBool(q.Get("ignore_rejected"))
				if err != nil {
					return result(400, map[string]string{"error": "ignore_rejected must be true or false"})
				}
			}
			if q.Get("from") != "" {
				f.From, err = time.Parse(time.RFC3339, q.Get("from"))
				if err != nil {
					return result(400, map[string]string{"error": "from must be RFC3339"})
				}
			}
			if q.Get("to") != "" {
				f.To, err = time.Parse(time.RFC3339, q.Get("to"))
				if err != nil {
					return result(400, map[string]string{"error": "to must be RFC3339"})
				}
			}
			f.Offset, _ = strconv.Atoi(q.Get("offset"))
			f.Limit, _ = strconv.Atoi(q.Get("limit"))
			f.MinDuration, _ = strconv.ParseFloat(q.Get("min_duration"), 64)
			p, err := e.List(f)
			if err != nil {
				return result(500, map[string]string{"error": "database read failed"})
			}
			return result(200, p)
		case "/record":
			r, err := e.Get(req.Query.Get("id"))
			if errors.Is(err, ErrNotFound) {
				return result(404, map[string]string{"error": "record not found"})
			}
			if err != nil {
				return result(500, map[string]string{"error": "database read failed"})
			}
			messages := conversationMessages(r)
			if r.Images == nil {
				if !e.images.pending(r.ID) {
					e.images.enqueue(r.ID)
				}
			} else {
				for _, im := range r.Images {
					if im.Status == "pending" || im.Status == "ready" && !e.images.exists(im) {
						if !e.images.pending(r.ID) {
							e.images.enqueue(r.ID)
						}
						break
					}
				}
			}
			e.images.decorate(messages, r.Images)
			if req.Query.Get("view") == "chat" {
				// Base64 originals must not travel in hidden raw fields when the
				// browser only needs the conversation and WebP thumbnails.
				for _, body := range recordBodyFields(r) {
					*body = ""
				}
			}
			return result(200, map[string]any{"record": r, "conversation": messages})
		case "/images":
			id := req.Query.Get("record_id")
			assets, err := e.store.GetImages(id)
			if err != nil {
				return result(404, map[string]string{"error": "record not found"})
			}
			needs := len(assets) == 0
			for _, asset := range assets {
				if asset.Status == "pending" {
					needs = true
				}
			}
			if needs && !e.images.pending(id) {
				e.images.enqueue(id)
			}
			return result(200, map[string]any{"images": assets, "pending": e.images.pending(id), "limit": e.cfg.MaxImagesPerRecord})
		case "/image":
			return e.imageResponse(req)
		}
	}
	if req.Method == "POST" && path == "/settings" {
		var body settingsUpdate
		if len(req.Body) > 1024 || json.Unmarshal(req.Body, &body) != nil {
			return result(400, map[string]string{"error": "invalid settings"})
		}
		if err := body.validate(); err != nil {
			return result(400, map[string]string{"error": err.Error()})
		}
		if err := e.store.SetSettings(body); err != nil {
			return result(500, map[string]string{"error": "settings write failed"})
		}
		return result(200, e.store.Settings(e.cfg.RefreshIntervalSeconds))
	}
	if req.Method == "POST" && path == "/images/retry" {
		id := req.Query.Get("record_id")
		if len(id) == 0 || len(id) > 256 {
			return result(400, map[string]string{"error": "invalid record_id"})
		}
		if err := e.images.retry(id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return result(404, map[string]string{"error": "record not found"})
			}
			return result(503, map[string]string{"error": err.Error()})
		}
		return result(202, map[string]bool{"queued": true})
	}
	if req.Method == "POST" && path == "/aliases/sync" {
		if len(req.Body) > 2<<20 {
			return result(413, map[string]string{"error": "body too large"})
		}
		var body struct {
			Aliases map[string]string `json:"aliases"`
		}
		if json.Unmarshal(req.Body, &body) != nil || body.Aliases == nil {
			return result(400, map[string]string{"error": "aliases object required"})
		}
		if err := e.store.SyncPanelAliases(body.Aliases); err != nil {
			return result(400, map[string]string{"error": err.Error()})
		}
		return result(200, map[string]any{"synced": len(body.Aliases), "manual_aliases_preserved": true})
	}
	if req.Method == "POST" && path == "/aliases" {
		if len(req.Body) > 8192 {
			return result(413, map[string]string{"error": "body too large"})
		}
		var b struct {
			KeyID string `json:"key_id"`
			Key   string `json:"key"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal(req.Body, &b) != nil {
			return result(400, map[string]string{"error": "invalid JSON"})
		}
		if err := e.Alias(b.KeyID, b.Key, b.Alias); err != nil {
			return result(400, map[string]string{"error": err.Error()})
		}
		return result(200, map[string]bool{"ok": true})
	}
	return result(404, map[string]string{"error": "not found"})
}

func (e *Engine) imageResponse(req wire.ManagementRequest) wire.ManagementResponse {
	id, recordID := req.Query.Get("id"), req.Query.Get("record_id")
	variant := req.Query.Get("variant")
	if variant != "" && variant != "original" && variant != "preview" {
		return result(400, map[string]string{"error": "invalid image variant"})
	}
	if !validSHA256(id) || len(recordID) == 0 || len(recordID) > 256 {
		return result(400, map[string]string{"error": "invalid image reference"})
	}
	assets, err := e.store.GetImages(recordID)
	if err != nil {
		return result(404, map[string]string{"error": "record not found"})
	}
	for _, asset := range assets {
		if asset.ID != id {
			continue
		}
		if asset.Status != "ready" {
			return result(409, map[string]string{"error": "image is not archived", "status": asset.Status})
		}
		var data []byte
		mime, name, disposition := asset.MIME, imageDownloadName(asset), "attachment"
		if variant == "preview" {
			data, err = e.images.preview(asset)
			mime, name, disposition = "image/webp", asset.ID+".webp", "inline"
		} else {
			data, err = e.images.read(asset)
		}
		if errors.Is(err, errImageBusy) {
			return result(429, map[string]string{"error": "image readers busy; retry later"})
		}
		if err != nil && variant == "preview" {
			return result(422, map[string]string{"error": err.Error()})
		}
		if err != nil {
			_ = e.store.ImageFailed(recordID, id, "本地图片文件缺失或损坏，请重试服务端保存")
			return result(410, map[string]string{"error": "archived image unavailable or integrity check failed"})
		}
		return wire.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {mime}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; sandbox"}, "Cross-Origin-Resource-Policy": {"same-origin"}, "Content-Disposition": {disposition + "; filename=\"" + name + "\""}}, Body: data}
	}
	return result(404, map[string]string{"error": "image does not belong to this record"})
}
