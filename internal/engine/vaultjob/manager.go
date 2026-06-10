package vaultjob

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const maxConcurrentJobs = 100
const jobTTL = time.Hour

type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusError   Status = "error"
)

type Phase string

const (
	PhaseCopying  Phase = "copying"
	PhaseIndexing Phase = "indexing"
	PhaseDone     Phase = "done"
)

// Job represents a single vault clone or merge operation.
// ID, Operation, Source, Target, CopyTotal, IndexTotal, and StartedAt are
// write-once at creation time and safe to read without a lock.
// Status, Phase, Err, and CompletedAt are mutable and protected by mu.
// CopyCurrent and IndexCurrent use atomic operations.
type Job struct {
	// Immutable after Create — no lock needed.
	ID         string
	Operation  string // "clone" | "merge"
	Source     string
	Target     string
	CopyTotal  int64
	IndexTotal int64
	StartedAt  time.Time

	// Mutable fields — protected by mu.
	mu          sync.RWMutex
	status      Status
	phase       Phase
	err         string
	completedAt time.Time

	CopyCurrent  atomic.Int64
	IndexCurrent atomic.Int64
}

// Status returns the current job status safely.
func (j *Job) GetStatus() Status {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.status
}

// GetPhase returns the current job phase safely.
func (j *Job) GetPhase() Phase {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.phase
}

// GetErr returns the error string safely.
func (j *Job) GetErr() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.err
}

// GetCompletedAt returns the completion time safely.
func (j *Job) GetCompletedAt() time.Time {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.completedAt
}

// SetPhase sets the job phase. Exported so engine_clone.go can call it.
// Thread-safe.
func (j *Job) SetPhase(p Phase) {
	j.mu.Lock()
	j.phase = p
	j.mu.Unlock()
}

// Pct returns the combined completion percentage across both phases, clamped to 100.
func (j *Job) Pct() float64 {
	total := float64(j.CopyTotal + j.IndexTotal)
	if total == 0 {
		return 0
	}
	done := float64(j.CopyCurrent.Load() + j.IndexCurrent.Load())
	result := done / total * 100
	if result > 100 {
		result = 100
	}
	return result
}

// StatusSnapshot is a JSON-serializable point-in-time view of a Job.
type StatusSnapshot struct {
	JobID        string  `json:"job_id"`
	Operation    string  `json:"operation"`
	Status       string  `json:"status"`
	Phase        string  `json:"phase"`
	CopyTotal    int64   `json:"copy_total"`
	CopyCurrent  int64   `json:"copy_current"`
	IndexTotal   int64   `json:"index_total"`
	IndexCurrent int64   `json:"index_current"`
	Pct          float64 `json:"pct"`
	ElapsedMs    int64   `json:"elapsed_ms"`
	Error        string  `json:"error,omitempty"`
}

// Snapshot returns a StatusSnapshot of the job at this instant.
func (j *Job) Snapshot() StatusSnapshot {
	j.mu.RLock()
	status := j.status
	phase := j.phase
	errStr := j.err
	completedAt := j.completedAt
	j.mu.RUnlock()

	elapsed := time.Since(j.StartedAt)
	if !completedAt.IsZero() {
		elapsed = completedAt.Sub(j.StartedAt)
	}
	return StatusSnapshot{
		JobID:        j.ID,
		Operation:    j.Operation,
		Status:       string(status),
		Phase:        string(phase),
		CopyTotal:    j.CopyTotal,
		CopyCurrent:  j.CopyCurrent.Load(),
		IndexTotal:   j.IndexTotal,
		IndexCurrent: j.IndexCurrent.Load(),
		Pct:          j.Pct(),
		ElapsedMs:    elapsed.Milliseconds(),
		Error:        errStr,
	}
}

// Manager tracks active and completed vault jobs.
type Manager struct {
	jobs      sync.Map // jobID → *Job
	mu        sync.Mutex
	running   int
	nextID    uint64
	stop      chan struct{} // closed by Close() to terminate the gc goroutine
	closeOnce sync.Once
}

// NewManager creates a Manager and starts the background GC goroutine.
// Call Close() when the manager is no longer needed to stop the GC goroutine.
func NewManager() *Manager {
	m := &Manager{stop: make(chan struct{})}
	go m.gc()
	return m
}

// Close stops the background GC goroutine. Idempotent — safe to call multiple times.
// Must be called before the owning Engine is discarded to avoid a goroutine leak.
func (m *Manager) Close() {
	m.closeOnce.Do(func() { close(m.stop) })
}

// Create allocates a new Job. Returns an error if too many jobs are running.
func (m *Manager) Create(op, source, target string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running >= maxConcurrentJobs {
		return nil, fmt.Errorf("too many concurrent vault jobs (max %d)", maxConcurrentJobs)
	}
	m.nextID++
	j := &Job{
		ID:        strconv.FormatUint(m.nextID, 10),
		Operation: op,
		Source:    source,
		Target:    target,
		StartedAt: time.Now(),
	}
	j.status = StatusRunning
	j.phase = PhaseCopying
	m.jobs.Store(j.ID, j)
	m.running++
	return j, nil
}

// Complete marks a job as done.
func (m *Manager) Complete(j *Job) {
	j.mu.Lock()
	j.status = StatusDone
	j.phase = PhaseDone
	j.completedAt = time.Now()
	j.mu.Unlock()

	m.mu.Lock()
	m.running--
	m.mu.Unlock()
}

// Fail marks a job as errored.
func (m *Manager) Fail(j *Job, err error) {
	j.mu.Lock()
	j.status = StatusError
	j.err = err.Error()
	j.completedAt = time.Now()
	j.mu.Unlock()

	m.mu.Lock()
	m.running--
	m.mu.Unlock()
}

// HasActiveJobTargeting returns true if any currently-running job lists vaultName
// as its Target. Used to guard vault deletion: a vault being written to by an
// active clone/merge must not be deleted mid-operation.
// Deleting a source vault is not blocked here — the merge's own cleanup path
// calls DeleteVault on the source after the copy phase, which must be allowed.
func (m *Manager) HasActiveJobTargeting(vaultName string) bool {
	found := false
	m.jobs.Range(func(_, v any) bool {
		j := v.(*Job)
		if j.GetStatus() == StatusRunning && j.Target == vaultName {
			found = true
			return false // stop iteration
		}
		return true
	})
	return found
}

// Get returns the job with the given ID, or (nil, false) if not found.
func (m *Manager) Get(jobID string) (*Job, bool) {
	v, ok := m.jobs.Load(jobID)
	if !ok {
		return nil, false
	}
	return v.(*Job), true
}

// gc periodically evicts completed jobs older than jobTTL.
// Exits when m.stop is closed via Close().
func (m *Manager) gc() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			m.jobs.Range(func(k, v any) bool {
				j := v.(*Job)
				j.mu.RLock()
				s := j.status
				completedAt := j.completedAt
				j.mu.RUnlock()
				if s != StatusRunning && !completedAt.IsZero() && now.Sub(completedAt) > jobTTL {
					m.jobs.Delete(k)
				}
				return true
			})
		case <-m.stop:
			return
		}
	}
}
