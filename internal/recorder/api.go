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

func Registration() map[string]any {
	return map[string]any{"schema_version": 6, "metadata": map[string]any{
		"Name": PluginID, "Version": "0.1.0", "Author": "txsdx", "GitHubRepository": "https://github.com/txsdx/cpa-plugin-request-recorder",
		"ConfigFields": []map[string]string{
			{"Name": "db_path", "Type": "string", "Description": "持久化数据库路径，变更需重启"},
			{"Name": "logs_dir", "Type": "string", "Description": "CPA request-log 目录，用于关联上游原文"},
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
	for _, p := range [][2]string{{"GET", "/records"}, {"GET", "/record"}, {"GET", "/status"}, {"POST", "/aliases"}} {
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
		return wire.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {mime}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: https: http:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"}, "Referrer-Policy": {"no-referrer"}}, Body: b}
	}
	if !strings.HasPrefix(req.Path, APIBase+"/") {
		return result(404, map[string]string{"error": "not found"})
	}
	path := strings.TrimPrefix(req.Path, APIBase)
	if req.Method == "GET" {
		switch path {
		case "/status":
			return result(200, e.Status())
		case "/records":
			q := req.Query
			f := Filter{Query: q.Get("q"), KeyID: q.Get("key_id"), Alias: q.Get("alias"), IP: q.Get("ip"), Model: q.Get("model"), Provider: q.Get("provider"), Outcome: q.Get("outcome"), Session: q.Get("session"), Reasoning: q.Get("reasoning")}
			var err error
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
			return result(200, map[string]any{"record": r, "conversation": Conversation(r.RequestBody, r.ResponseBody), "upstream_conversation": UpstreamConversation(r)})
		}
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
