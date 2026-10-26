package recorder

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var tracePattern = regexp.MustCompile(`^[a-fA-F0-9-]{8,64}$`)
var sectionPattern = regexp.MustCompile(`(?m)^=== (REQUEST INFO|HEADERS|REQUEST BODY|API REQUEST(?: [0-9]+)?|API RESPONSE(?: [0-9]+)?|API ERROR RESPONSE|RESPONSE|WEBSOCKET TIMELINE|API WEBSOCKET TIMELINE) ===\r?$`)

type LogParts struct {
	Request, UpstreamRequests, UpstreamResponses string
	Attempt                                      int
	Truncated                                    bool
}

// CPA text logs provide the last upstream attempt. Only request bodies and
// assembled final responses are retained; section wrappers and earlier attempts are discarded.
func ParseLog(raw string) (LogParts, error) {
	var p LogParts
	locs := sectionPattern.FindAllStringSubmatchIndex(raw, -1)
	hasResponse := false
	requests := map[int]string{}
	lastIndex := -1
	for i, loc := range locs {
		name := raw[loc[2]:loc[3]]
		end := len(raw)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.TrimLeft(raw[loc[1]:end], "\r\n")
		switch {
		case name == "REQUEST BODY":
			p.Request = strings.TrimRight(body, "\r\n")
		case strings.HasPrefix(name, "API REQUEST"):
			index := 0
			_, _ = fmt.Sscanf(name, "API REQUEST %d", &index)
			if _, value, ok := strings.Cut(body, "Body:\n"); ok {
				requests[index] = strings.TrimSpace(value)
			}
		case strings.HasPrefix(name, "API RESPONSE"):
			index := 0
			_, _ = fmt.Sscanf(name, "API RESPONSE %d", &index)
			if _, value, ok := strings.Cut(body, "Body:\n"); ok && index >= lastIndex {
				lastIndex = index
				p.UpstreamResponses, _ = FinalResponse(strings.TrimSpace(value))
			}
		case name == "RESPONSE":
			hasResponse = true
		}
	}
	p.Attempt = lastIndex
	p.UpstreamRequests = requests[lastIndex]
	if !hasResponse {
		return p, errors.New("CPA 日志尚未写入终结 RESPONSE 区段")
	}
	p.Truncated = strings.Contains(raw, "TRUNCATED") || strings.Contains(raw, "CAPTURE INCOMPLETE")
	return p, nil
}
func findLog(dir, trace string, started time.Time) (string, error) {
	if !tracePattern.MatchString(trace) {
		return "", errors.New("宿主未提供可用于关联日志的 trace ID")
	}
	short := trace
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	matches := []string{}
	for _, f := range files {
		if f.IsDir() || f.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(f.Name(), ".log") {
			continue
		}
		// CPA uses <path>-<timestamp>-<short_trace>[-collision].log.
		base := strings.TrimSuffix(f.Name(), ".log")
		if !strings.HasSuffix(base, "-"+short) && !regexp.MustCompile(`-`+regexp.QuoteMeta(short)+`-[0-9]+$`).MatchString(base) {
			continue
		}
		info, err := f.Info()
		if err != nil {
			return "", err
		}
		if info.ModTime().Before(started.Add(-time.Minute)) {
			continue
		}
		matches = append(matches, filepath.Join(dir, f.Name()))
	}
	if len(matches) > 1 {
		return "", errors.New("短 trace ID 匹配多个日志，拒绝错误关联")
	}
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	return matches[0], nil
}
func (e *Engine) enrich(id string, j *logJob) {
	path, err := findLog(e.cfg.LogsDir, j.trace, j.started)
	var p LogParts
	if err == nil {
		// Do not follow a substituted symlink out of logs_dir.
		var root *os.Root
		root, err = os.OpenRoot(e.cfg.LogsDir)
		if err == nil {
			var f *os.File
			f, err = root.Open(filepath.Base(path))
			if err == nil {
				info, ie := f.Stat()
				if ie != nil {
					err = ie
				} else if !info.Mode().IsRegular() {
					err = errors.New("not a regular log file")
				} else if info.Size() > int64(e.cfg.MaxBodyBytes)*4 {
					err = fmt.Errorf("CPA 日志超过 %d 字节导入限制", e.cfg.MaxBodyBytes*4)
				} else {
					if path != j.path || info.Size() != j.size {
						j.path = path
						j.size = info.Size()
						j.stable = 0
						err = errors.New("等待 CPA 日志写入完成")
					} else {
						j.stable++
						if j.stable < 2 {
							err = errors.New("等待 CPA 日志稳定")
						} else {
							var b []byte
							b, err = io.ReadAll(io.LimitReader(f, int64(e.cfg.MaxBodyBytes)*4+1))
							if err == nil {
								p, err = ParseLog(string(b))
							}
						}
					}
				}
				f.Close()
			}
			root.Close()
		}
	}
	if err == nil && p.UpstreamResponses == "" {
		err = errors.New("日志中无上游 API RESPONSE，可能为执行前失败或宿主未记录")
	}
	if err != nil && time.Now().Before(j.deadline) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.jobs[id] != j {
		return
	}
	r, getErr := e.store.Get(id)
	if getErr != nil {
		if !errors.Is(getErr, ErrNotFound) {
			e.error(getErr)
		}
		delete(e.jobs, id)
		return
	}
	if err != nil {
		r.UpstreamState = "unavailable"
		r.UpstreamError = err.Error()
	} else {
		r.UpstreamAttempt = p.Attempt
		r.UpstreamRequests = p.UpstreamRequests
		r.UpstreamResponses = p.UpstreamResponses
		r.UpstreamState = "log_captured"
		r.UpstreamError = ""
		if p.Request != "" {
			r.RequestBody = p.Request
			r.RequestSource = "cpa_request_log"
		}
		if p.Truncated {
			r.UpstreamState = "truncated"
			addWarning(r, "CPA 源日志含截断或采集不完整标记")
		}
	}
	updatePreviews(r)
	if saveErr := e.store.Save(r); saveErr != nil {
		e.error(saveErr)
		return
	}
	delete(e.jobs, id)
	e.images.enqueue(id)
}
