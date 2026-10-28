package recorder

import (
	"errors"
	"strings"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
)

const pendingUsageTTL = 5 * time.Minute
const recentUsageTTL = 10 * time.Minute
const maxRecentUsage = 2048
const maxUsageOwners = 8192

type pendingUsage struct {
	record     wire.Usage
	receivedAt time.Time
}

// Only small execution identifiers and accounting counters are retained, never
// old prompts, responses, images, or complete request records.
type usageExecution struct {
	summary   Summary
	attempts  map[string]int64
	known     bool
	expiresAt time.Time
}

func (e *Engine) Usage(u wire.Usage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if u.RequestID == "" {
		e.dropUsage(time.Now())
		return nil
	}
	matched, err := e.joinUsage(u)
	if err != nil {
		return err
	}
	if matched {
		delete(e.pendingUsage, u.RequestID)
		return nil
	}
	if pending, exists := e.pendingUsage[u.RequestID]; exists {
		pending.record = u
		e.pendingUsage[u.RequestID] = pending
	} else if len(e.pendingUsage) < 4096 {
		e.pendingUsage[u.RequestID] = pendingUsage{record: u, receivedAt: time.Now()}
	} else {
		e.dropUsage(time.Now())
	}
	return nil
}

func (e *Engine) bindUsageOwner(reporter, execution string) {
	if len(e.usageOwners) < maxUsageOwners || e.usageOwners[reporter] != "" {
		e.usageOwners[reporter] = execution
	}
}

func (e *Engine) forgetUsage(id string) {
	r := e.recentUsage[id]
	if r == nil {
		return
	}
	delete(e.recentUsage, id)
	index := e.recentUsageTraces[r.summary.TraceID]
	delete(index, id)
	if len(index) == 0 {
		delete(e.recentUsageTraces, r.summary.TraceID)
	}
	for reporter := range r.attempts {
		if e.usageOwners[reporter] == id {
			delete(e.usageOwners, reporter)
		}
	}
}

func (e *Engine) rememberUsage(r *Record) {
	if old := e.recentUsage[r.RequestID]; old != nil {
		if old.summary.ID != r.ID {
			for _, recent := range e.recentUsage {
				if recent.summary.ID == old.summary.ID {
					recent.summary.ID = r.ID
				}
			}
		}
		e.forgetUsage(r.RequestID)
	}
	for len(e.recentUsage) >= maxRecentUsage || len(e.usageOwners)+len(r.UsageAttempts) > maxUsageOwners {
		oldest := ""
		for id, recent := range e.recentUsage {
			if oldest == "" || recent.expiresAt.Before(e.recentUsage[oldest].expiresAt) {
				oldest = id
			}
		}
		if oldest == "" {
			return
		}
		e.forgetUsage(oldest)
	}
	ref := &usageExecution{
		summary:  Summary{ID: r.ID, RequestID: r.RequestID, TraceID: r.TraceID, KeyID: r.KeyID, Model: r.Model, RequestedModel: r.RequestedModel, ResponseModel: r.ResponseModel, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt},
		attempts: map[string]int64{}, known: r.Tokens != nil, expiresAt: time.Now().Add(recentUsageTTL),
	}
	e.recentUsage[r.RequestID] = ref
	if e.recentUsageTraces[r.TraceID] == nil {
		e.recentUsageTraces[r.TraceID] = map[string]*usageExecution{}
	}
	e.recentUsageTraces[r.TraceID][r.RequestID] = ref
	for _, attempt := range r.UsageAttempts {
		ref.attempts[attempt.ID] = attempt.Tokens.TotalTokens
		e.bindUsageOwner(attempt.ID, r.RequestID)
	}
}

func (e *Engine) applyCurrentUsage(r *Record, u wire.Usage) error {
	applyUsage(r, u)
	if err := e.store.Save(r); err != nil {
		return err
	}
	e.rememberUsage(r)
	return nil
}

func (e *Engine) applyUsageOwner(id string, u wire.Usage) (bool, error) {
	if active := e.active[id]; active != nil {
		applyUsage(&active.record, u)
		e.bindUsageOwner(u.RequestID, id)
		return true, nil
	}
	ref := e.recentUsage[id]
	if ref == nil {
		return false, nil
	}
	_, seen := ref.attempts[u.RequestID]
	if !seen && (len(ref.attempts) >= 1024 || len(e.usageOwners) >= maxUsageOwners) {
		e.dropUsage(time.Now())
		return true, nil
	}
	delta := u.Detail.TotalTokens - ref.attempts[u.RequestID]
	current, err := e.store.addDelayedUsage(ref.summary.ID, id, delta, !ref.known)
	if errors.Is(err, ErrNotFound) {
		e.forgetUsage(id)
		e.dropUsage(time.Now())
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if current {
		r, err := e.store.Get(ref.summary.ID)
		if err != nil {
			return false, err
		}
		return true, e.applyCurrentUsage(r, u)
	}
	ref.attempts[u.RequestID] = u.Detail.TotalTokens
	ref.known = true
	if ref.summary.KeyID == "" && u.APIKey != "" {
		ref.summary.KeyID = fingerprint(u.APIKey)
	}
	e.bindUsageOwner(u.RequestID, id)
	return true, nil
}

func usageCompatible(r Summary, u wire.Usage) bool {
	if r.KeyID != "" && u.APIKey != "" && r.KeyID != fingerprint(u.APIKey) {
		return false
	}
	if !u.RequestedAt.IsZero() {
		if !r.StartedAt.IsZero() && u.RequestedAt.Before(r.StartedAt) {
			return false
		}
		if !r.CompletedAt.IsZero() && u.RequestedAt.After(r.CompletedAt) {
			return false
		}
	}
	return true
}

func usageModelsMatch(r Summary, u wire.Usage) bool {
	hasRequestModel, hasUsageModel := false, false
	for _, left := range []string{r.Model, r.RequestedModel, r.ResponseModel} {
		if strings.TrimSpace(left) != "" {
			hasRequestModel = true
		}
		for _, right := range []string{u.Model, u.Alias, u.ResponseModel} {
			if strings.TrimSpace(right) != "" {
				hasUsageModel = true
			}
			if strings.TrimSpace(left) != "" && strings.TrimSpace(left) == strings.TrimSpace(right) {
				return true
			}
		}
	}
	return !hasRequestModel || !hasUsageModel
}

// Exact lifecycle IDs and previously observed reporter ownership take priority.
// Trace matches must be unique; time and aliases only narrow that exact trace.
func (e *Engine) joinUsage(u wire.Usage) (bool, error) {
	if e.active[u.RequestID] != nil {
		return e.applyUsageOwner(u.RequestID, u)
	}
	if r, err := e.store.Get(u.RequestID); err == nil {
		if r.RequestID == "" || r.RequestID == u.RequestID {
			return true, e.applyCurrentUsage(r, u)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if id := e.usageOwners[u.RequestID]; id != "" {
		if handled, err := e.applyUsageOwner(id, u); handled || err != nil {
			return handled, err
		}
	}
	if e.recentUsage[u.RequestID] != nil {
		return e.applyUsageOwner(u.RequestID, u)
	}
	if u.TraceID == "" {
		return false, nil
	}
	candidates := map[string]Summary{}
	for id, ref := range e.recentUsageTraces[u.TraceID] {
		candidates[id] = ref.summary
	}
	stored, err := e.store.ByTrace(u.TraceID)
	if err != nil {
		return false, err
	}
	for _, r := range stored {
		candidates[r.RequestID] = r
	}
	for id, active := range e.active {
		if active.record.TraceID == u.TraceID {
			candidates[id] = active.record.Summary
		}
	}
	for id, r := range candidates {
		if !usageCompatible(r, u) {
			delete(candidates, id)
		}
	}
	if len(candidates) > 1 {
		for id, r := range candidates {
			if !usageModelsMatch(r, u) {
				delete(candidates, id)
			}
		}
	}
	if len(candidates) != 1 {
		return false, nil
	}
	for id, r := range candidates {
		if e.active[id] != nil || e.recentUsage[id] != nil {
			return e.applyUsageOwner(id, u)
		}
		record, err := e.store.Get(r.ID)
		if err != nil {
			return false, err
		}
		return true, e.applyCurrentUsage(record, u)
	}
	return false, nil
}

func (e *Engine) drainUsage() {
	for id, pending := range e.pendingUsage {
		matched, err := e.joinUsage(pending.record)
		if err != nil {
			e.error(err)
		}
		if matched {
			delete(e.pendingUsage, id)
		}
	}
}

func (e *Engine) dropUsage(now time.Time) { e.discardedUsage++; e.lastUsageDrop = now }

func (e *Engine) expireUsage(now time.Time) {
	for id, pending := range e.pendingUsage {
		if now.Sub(pending.receivedAt) > pendingUsageTTL {
			delete(e.pendingUsage, id)
			e.dropUsage(now)
		}
	}
	for id, ref := range e.recentUsage {
		if !now.Before(ref.expiresAt) {
			e.forgetUsage(id)
		}
	}
	for reporter, id := range e.usageOwners {
		if e.active[id] == nil && e.recentUsage[id] == nil {
			delete(e.usageOwners, reporter)
		}
	}
}
