package recorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

type Config struct {
	DBPath         string `yaml:"db_path"`
	LogsDir        string `yaml:"logs_dir"`
	RetentionDays  int    `yaml:"retention_days"`
	MaxRecords     int    `yaml:"max_records"`
	MaxBodyBytes   int    `yaml:"max_body_bytes"`
	MaxInflight    int    `yaml:"max_inflight"`
	MaxMemoryBytes int    `yaml:"max_memory_bytes"`
}

func DefaultConfig() Config {
	return Config{DBPath: "data/request-recorder.db", RetentionDays: 30, MaxRecords: 100000, MaxBodyBytes: 64 << 20, MaxInflight: 256, MaxMemoryBytes: 256 << 20}
}
func (c Config) Validate() error {
	if c.DBPath == "" || c.RetentionDays < 0 || c.MaxRecords < 0 || c.MaxBodyBytes < 1024 || c.MaxInflight < 1 || c.MaxMemoryBytes < c.MaxBodyBytes {
		return errors.New("invalid config: db_path required; retention/max_records >= 0; max_body_bytes >= 1024; max_inflight >= 1; max_memory_bytes >= max_body_bytes")
	}
	return nil
}

type active struct {
	record   Record
	response bytes.Buffer
	size     int
}
type logJob struct {
	trace             string
	started, deadline time.Time
	path              string
	size              int64
	stable            int
}
type Engine struct {
	mu           sync.Mutex
	store        *Store
	cfg          Config
	active       map[string]*active
	pendingUsage map[string]wire.Usage
	skipped      map[string]time.Time
	jobs         map[string]*logJob
	used         int
	dropped      uint64
	lastError    string
	stop         chan struct{}
	done         chan struct{}
}

func New(c Config) (*Engine, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	s, err := Open(c.DBPath)
	if err != nil {
		return nil, err
	}
	e := &Engine{store: s, cfg: c, active: map[string]*active{}, jobs: map[string]*logJob{}, pendingUsage: map[string]wire.Usage{}, skipped: map[string]time.Time{}, stop: make(chan struct{}), done: make(chan struct{})}
	if err = s.Prune(c.RetentionDays, c.MaxRecords); err != nil {
		s.Close()
		return nil, err
	}
	// Resume log enrichment after a host restart; terminal records are durable.
	pending, err := s.PendingLogs()
	if err != nil {
		s.Close()
		return nil, err
	}
	for _, r := range pending {
		e.jobs[r.ID] = &logJob{trace: r.TraceID, started: r.StartedAt, deadline: time.Now().Add(30 * time.Second)}
	}
	go e.maintain()
	return e, nil
}
func (e *Engine) Close() error {
	close(e.stop)
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, a := range e.active {
		a.record.Outcome = "incomplete"
		a.record.CompletedAt = time.Now()
		a.record.DurationMS = ms(time.Since(a.record.StartedAt))
		addWarning(&a.record, "插件关闭前未收到结束事件")
		e.persist(a)
		delete(e.active, id)
	}
	return e.store.Close()
}
func (e *Engine) error(err error) {
	if err != nil {
		e.lastError = err.Error()
		log.Printf("request-recorder storage/capture error: %v", err)
	}
}
func (e *Engine) ensure(id string) *active {
	if id == "" {
		return nil
	}
	if a := e.active[id]; a != nil {
		return a
	}
	if _, skipped := e.skipped[id]; skipped {
		return nil
	}
	if len(e.active) >= e.cfg.MaxInflight {
		e.dropped++
		e.skipped[id] = time.Now()
		return nil
	}
	a := &active{record: Record{Summary: Summary{ID: id, StartedAt: time.Now(), Outcome: "pending", UpstreamState: "unavailable", Warnings: []string{}}, RequestSource: "interceptor", ResponseSource: "downstream_interceptor"}}
	e.active[id] = a
	return a
}
func (e *Engine) capture(a *active, b []byte) string {
	n := len(b)
	if n > e.cfg.MaxBodyBytes {
		n = e.cfg.MaxBodyBytes
	}
	if remain := e.cfg.MaxMemoryBytes - e.used; n > remain {
		n = remain
	}
	if n < 0 {
		n = 0
	}
	if n < len(b) {
		addWarning(&a.record, "请求内容达到采集容量限制，记录已截断")
	}
	s := string(b[:n])
	a.size += n
	e.used += n
	return s
}
func (e *Engine) Request(req wire.Request, after bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.ensure(req.RequestID)
	if a == nil {
		return nil
	}
	r := &a.record
	if !after && r.RequestBody == "" {
		r.RequestBody = e.capture(a, req.Body)
		r.RequestHeaders = safeHeaders(req.Headers)
	}
	if after {
		e.used -= len(r.InterceptedBody)
		a.size -= len(r.InterceptedBody)
		r.InterceptedBody = e.capture(a, req.Body)
	}
	r.TraceID = req.TraceID
	r.Format = req.SourceFormat
	r.Stream = req.Stream
	r.RequestedModel = req.RequestedModel
	r.Model = req.Model
	r.CallerScope = req.CallerScope
	e.metadata(r, req.Metadata)
	e.drainUsage()
	if r.RequestedModel == "" {
		r.RequestedModel = req.Model
	}
	if r.ReasoningEffort == "" {
		var b map[string]any
		_ = json.Unmarshal(req.Body, &b)
		r.ReasoningEffort = str(b["reasoning_effort"])
		if rr, ok := b["reasoning"].(map[string]any); ok {
			r.ReasoningEffort = str(rr["effort"])
		}
		if th, ok := b["thinking"].(map[string]any); ok && r.ReasoningEffort == "" {
			r.ReasoningEffort = fmt.Sprint(th)
		}
	}
	return nil
}
func (e *Engine) metadata(r *Record, m map[string]any) {
	for _, p := range []struct {
		dst  *string
		keys []string
	}{
		{&r.SessionID, []string{"canonical_session_id", "execution_session_id", "session_id"}},
		{&r.ParentSessionID, []string{"parent_session_id"}}, {&r.ClientIP, []string{"resolved_client_ip", "client_ip"}},
		{&r.UserAgent, []string{"user_agent"}}, {&r.AuthID, []string{"selected_auth_id"}},
		{&r.ReasoningEffort, []string{"reasoning_effort"}}, {&r.CallerScope, []string{"caller_scope"}},
	} {
		for _, k := range p.keys {
			if v := str(m[k]); v != "" {
				*p.dst = v
				break
			}
		}
	}
}
func str(a any) string { s, _ := a.(string); return s }
func (e *Engine) Response(req wire.Response, stream bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.ensure(req.RequestID)
	if a == nil {
		return nil
	}
	r := &a.record
	if len(req.OriginalRequest) > 0 && r.RequestSource != "cpa_request_log" {
		e.used -= len(r.RequestBody)
		a.size -= len(r.RequestBody)
		r.RequestBody = e.capture(a, req.OriginalRequest)
		r.RequestSource = "host_original_request"
	}
	r.ResponseHeaders = safeHeaders(req.ResponseHeaders)
	if req.StatusCode > 0 {
		r.Status = req.StatusCode
	}
	e.metadata(r, req.Metadata)
	if stream && req.ChunkIndex < 0 {
		return nil
	}
	if len(req.Body) > 0 {
		if r.FirstChunkMS == nil {
			v := ms(time.Since(r.StartedAt))
			r.FirstChunkMS = &v
		}
		remaining := e.cfg.MaxBodyBytes - a.response.Len()
		n := len(req.Body)
		if n > remaining {
			n = remaining
		}
		if n > e.cfg.MaxMemoryBytes-e.used {
			n = e.cfg.MaxMemoryBytes - e.used
		}
		if n < 0 {
			n = 0
		}
		if n < len(req.Body) {
			addWarning(r, "响应达到采集容量限制，记录已截断")
		}
		a.response.Write(req.Body[:n])
		a.size += n
		e.used += n
	}
	return nil
}
func applyUsage(r *Record, u wire.Usage) {
	denominator := u.Detail.InputTokens
	if u.Provider == "claude" || strings.Contains(strings.ToLower(u.ExecutorType), "claude") {
		denominator += u.Detail.CacheReadTokens + u.Detail.CacheCreationTokens
	}
	attempt := UsageAttempt{ID: u.RequestID, Model: u.Model, Provider: u.Provider, AuthID: u.AuthID, Failed: u.Failed, RequestedAt: u.RequestedAt, DurationMS: ms(u.Latency), TTFTMS: ms(u.TTFT), Tokens: u.Detail, InputDenominator: denominator}
	updated := false
	for i := range r.UsageAttempts {
		if r.UsageAttempts[i].ID == u.RequestID {
			r.UsageAttempts[i] = attempt
			updated = true
			break
		}
	}
	if !updated {
		if len(r.UsageAttempts) < 1024 {
			r.UsageAttempts = append(r.UsageAttempts, attempt)
		} else {
			addWarning(r, "usage 尝试数超过 1024，后续用量未聚合")
		}
	}
	if u.TraceID != "" {
		r.TraceID = u.TraceID
	}
	r.Provider = u.Provider
	r.Model = u.Model
	r.ResponseModel = u.ResponseModel
	r.AuthID = u.AuthID
	if u.APIKey != "" {
		r.KeyID = fingerprint(u.APIKey)
		r.KeyHint = keyHint(u.APIKey)
		warnings := r.Warnings[:0]
		for _, w := range r.Warnings {
			if w != "宿主尚未提供 usage 中的 API key 信息" {
				warnings = append(warnings, w)
			}
		}
		r.Warnings = warnings
	}
	if u.SessionID != "" {
		r.SessionID = u.SessionID
	}
	r.ParentSessionID = u.ParentSessionID
	if u.ReasoningEffort != "" {
		r.ReasoningEffort = u.ReasoningEffort
	}
	r.ServiceTier = u.EffectiveServiceTier
	if u.ResponseServiceTier != "" {
		r.ServiceTier = u.ResponseServiceTier
	}
	t := wire.Tokens{}
	den := int64(0)
	cached := int64(0)
	for _, a := range r.UsageAttempts {
		t.InputTokens += a.Tokens.InputTokens
		t.OutputTokens += a.Tokens.OutputTokens
		t.ReasoningTokens += a.Tokens.ReasoningTokens
		t.CachedTokens += a.Tokens.CachedTokens
		t.CacheReadTokens += a.Tokens.CacheReadTokens
		t.CacheCreationTokens += a.Tokens.CacheCreationTokens
		t.TotalTokens += a.Tokens.TotalTokens
		den += a.InputDenominator
		read := a.Tokens.CacheReadTokens
		if read == 0 {
			read = a.Tokens.CachedTokens
		}
		cached += read
	}
	r.Tokens = &t
	r.CacheRatio = nil
	if den > 0 {
		ratio := float64(cached) / float64(den)
		r.CacheRatio = &ratio
	}
	if u.TTFT > 0 {
		v := ms(u.TTFT)
		r.TTFTMS = &v
	}
	if !u.RequestedAt.IsZero() && r.CompletedAt.IsZero() {
		r.StartedAt = u.RequestedAt
	}
	if u.Latency > 0 && r.CompletedAt.IsZero() {
		r.DurationMS = ms(u.Latency)
	}
	if u.Failed && u.Failure.Body != "" {
		r.Error = u.Failure.Body
	}
}

// UsageReporter IDs and lifecycle execution IDs are distinct in current CPA.
// Join exact IDs first, then an unambiguous full trace (+ model if necessary).
func (e *Engine) Usage(u wire.Usage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	matched, err := e.joinUsage(u)
	if err != nil {
		return err
	}
	if !matched {
		if len(e.pendingUsage) < 4096 {
			e.pendingUsage[u.RequestID] = u
		} else {
			e.lastError = "usage correlation queue full"
		}
	}
	return nil
}
func (e *Engine) joinUsage(u wire.Usage) (bool, error) {
	if a := e.active[u.RequestID]; a != nil {
		applyUsage(&a.record, u)
		return true, nil
	}
	if r, err := e.store.Get(u.RequestID); err == nil {
		applyUsage(r, u)
		return true, e.store.Save(r)
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	candidates := map[string]Summary{}
	for id, a := range e.active {
		if u.TraceID != "" && a.record.TraceID == u.TraceID {
			candidates[id] = a.record.Summary
		}
	}
	stored, err := e.store.ByTrace(u.TraceID)
	if err != nil {
		return false, err
	}
	for _, r := range stored {
		candidates[r.ID] = r
	}
	if len(candidates) > 1 {
		for id, r := range candidates {
			if r.Model != u.Model && r.RequestedModel != u.Model {
				delete(candidates, id)
			}
		}
	}
	if len(candidates) != 1 {
		return false, nil
	}
	for id := range candidates {
		if a := e.active[id]; a != nil {
			applyUsage(&a.record, u)
			return true, nil
		}
		r, err := e.store.Get(id)
		if err != nil {
			return false, err
		}
		applyUsage(r, u)
		return true, e.store.Save(r)
	}
	return false, nil
}
func (e *Engine) drainUsage() {
	for id, u := range e.pendingUsage {
		matched, err := e.joinUsage(u)
		if err != nil {
			e.error(err)
		}
		if matched {
			delete(e.pendingUsage, id)
		}
	}
}
func (e *Engine) persist(a *active) error {
	a.record.ResponseBody = a.response.String()
	return e.store.Save(&a.record)
}
func (e *Engine) Complete(c wire.Completion) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, skipped := e.skipped[c.RequestID]; skipped {
		delete(e.skipped, c.RequestID)
		return nil
	}
	// Completion may be repeated by a host; do not overwrite a completed record.
	a := e.active[c.RequestID]
	if a == nil {
		if _, err := e.store.Get(c.RequestID); err == nil {
			return nil
		}
		a = e.ensure(c.RequestID)
	}
	if a == nil {
		return nil
	}
	r := &a.record
	r.Outcome = c.Outcome
	r.Status = c.StatusCode
	r.Error = c.Error
	if c.TraceID != "" {
		r.TraceID = c.TraceID
	}
	if !c.StartedAt.IsZero() {
		r.StartedAt = c.StartedAt
	}
	r.CompletedAt = c.CompletedAt
	if r.CompletedAt.IsZero() {
		r.CompletedAt = time.Now()
	}
	r.DurationMS = ms(r.CompletedAt.Sub(r.StartedAt))
	e.metadata(r, c.Metadata)
	if r.Outcome != "succeeded" {
		addWarning(r, "请求未成功结束，响应可能只有部分内容")
	}
	if r.KeyID == "" {
		addWarning(r, "宿主尚未提供 usage 中的 API key 信息")
	}
	if e.cfg.LogsDir != "" {
		r.UpstreamState = "pending"
	}
	if err := e.persist(a); err != nil {
		e.error(err)
		return err
	}
	e.used -= a.size
	delete(e.active, c.RequestID)
	if e.cfg.LogsDir != "" {
		if len(e.jobs) < 4096 {
			e.jobs[c.RequestID] = &logJob{trace: r.TraceID, started: r.StartedAt, deadline: time.Now().Add(30 * time.Second)}
		} else {
			r.UpstreamState = "unavailable"
			r.UpstreamError = "日志补全队列已满"
			e.error(e.store.Save(r))
		}
	}
	return nil
}
func (e *Engine) maintain() {
	defer close(e.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.mu.Lock()
			ticks++
			for id, started := range e.skipped {
				if time.Since(started) > 30*time.Minute {
					delete(e.skipped, id)
				}
			}
			e.drainUsage()
			for id, u := range e.pendingUsage {
				if !u.RequestedAt.IsZero() && time.Since(u.RequestedAt) > 5*time.Minute {
					delete(e.pendingUsage, id)
					e.lastError = "usage 无法唯一关联到请求，未写入其他请求"
				}
			}
			for id, a := range e.active {
				if time.Since(a.record.StartedAt) > 30*time.Minute {
					a.record.Outcome = "incomplete"
					a.record.CompletedAt = time.Now()
					addWarning(&a.record, "30 分钟未收到结束事件")
					if err := e.persist(a); err != nil {
						e.error(err)
						continue
					}
					e.used -= a.size
					delete(e.active, id)
				}
			}
			jobs := make(map[string]*logJob, len(e.jobs))
			for id, j := range e.jobs {
				jobs[id] = j
			}
			if ticks%60 == 0 {
				e.error(e.store.Prune(e.cfg.RetentionDays, e.cfg.MaxRecords))
			}
			e.mu.Unlock()
			// Reading large CPA log files must not hold the capture lock.
			for id, j := range jobs {
				e.enrich(id, j)
			}
		}
	}
}
func (e *Engine) Status() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]any{"active": len(e.active), "pending_logs": len(e.jobs), "unmatched_usage": len(e.pendingUsage), "dropped_requests": e.dropped, "memory_bytes": e.used, "last_error": e.lastError, "logs_enabled": e.cfg.LogsDir != "", "db_path": e.cfg.DBPath, "retention_days": e.cfg.RetentionDays, "max_body_bytes": e.cfg.MaxBodyBytes, "capture_scope": "model executions reaching CPA request interceptors"}
}
func (e *Engine) Get(id string) (*Record, error) { return e.store.Get(id) }
func (e *Engine) List(f Filter) (Page, error)    { return e.store.List(f) }
func (e *Engine) Alias(keyID, key, alias string) error {
	if key != "" {
		keyID = fingerprint(strings.TrimSpace(key))
	}
	return e.store.SetAlias(keyID, strings.TrimSpace(alias))
}
