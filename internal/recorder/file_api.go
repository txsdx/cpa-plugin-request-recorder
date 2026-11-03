package recorder

import (
	"errors"
	"mime"
	"net/http"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

func (e *Engine) fileResponse(req wire.ManagementRequest) wire.ManagementResponse {
	id, recordID := req.Query.Get("id"), req.Query.Get("record_id")
	if !validSHA256(id) || len(recordID) == 0 || len(recordID) > 256 {
		return result(400, map[string]string{"error": "invalid file reference"})
	}
	assets, err := e.store.GetFiles(recordID)
	if err != nil {
		return result(404, map[string]string{"error": "record not found"})
	}
	for _, asset := range assets {
		if asset.ID != id {
			continue
		}
		if asset.Status != "ready" {
			return result(409, map[string]string{"error": "file is not archived", "status": asset.Status})
		}
		data, err := e.files.read(asset)
		if errors.Is(err, errImageBusy) {
			return result(429, map[string]string{"error": "file readers busy; retry later"})
		}
		if err != nil {
			_ = e.store.assetStore(true).failed(recordID, id, "本地文件缺失或损坏，请重试服务端保存")
			return result(410, map[string]string{"error": "archived file unavailable or integrity check failed"})
		}
		return wire.ManagementResponse{StatusCode: 200, Headers: http.Header{
			"Content-Type":        {"application/octet-stream"},
			"Content-Disposition": {mime.FormatMediaType("attachment", map[string]string{"filename": safeFileName(asset.Filename)})},
			"Cache-Control":       {"no-store"}, "X-Content-Type-Options": {"nosniff"},
			"Content-Security-Policy":      {"default-src 'none'; sandbox"},
			"Cross-Origin-Resource-Policy": {"same-origin"},
		}, Body: data}
	}
	return result(404, map[string]string{"error": "file does not belong to this record"})
}
