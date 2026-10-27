// Package nomadfake is an in-memory stand-in for the Nomad HTTP API, for
// tests. It implements the calls internal/nomad makes and records every
// request in full, so tests can search them for leaks:
//
//   - X-Nomad-Token is checked on every request (403 otherwise) and the
//     namespace query parameter must match the job's namespace;
//   - POST /v1/jobs honours EnforceIndex/JobModifyIndex like Nomad: index 0
//     means "must not exist", otherwise the index must be current;
//   - registering a different spec bumps the version and creates a fresh
//     allocation; registering the same spec is a no-op on allocations;
//   - allocation health is scripted: SetPhase, or HealthyAfter polls of the
//     allocations list, which models an instance that warms up;
//   - DELETE stops the job, with purge=true it forgets it.
package nomadfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Phase scripts what the allocation of the current version looks like.
type Phase string

const (
	Pending   Phase = "pending"
	Healthy   Phase = "healthy"
	Unhealthy Phase = "unhealthy"
	Failed    Phase = "failed"
)

// Request is one recorded request.
type Request struct {
	Method string
	Path   string
	Query  string
	Token  string
	Body   string
}

type job struct {
	raw         map[string]any
	spec        string // the job's sanholo.spec meta, or the raw JSON
	version     uint64
	modifyIndex uint64
	stop        bool
	phase       Phase
	polls       int
	allocSeq    int
	allocID     string
	restarts    int
	// old are allocations of earlier versions, still listed (and healthy)
	// until the job is stopped, as Nomad keeps them while it replaces them.
	old []map[string]any
}

// Fake is the server and its state.
type Fake struct {
	srv       *httptest.Server
	token     string
	namespace string

	mu       sync.Mutex
	jobs     map[string]*job
	reqs     []Request
	index    uint64
	override func(r Request) (status int, body string, ok bool)

	// HealthyAfter, when > 0, flips a pending allocation to healthy once
	// its allocations list has been read this many times.
	HealthyAfter int
	// OnRegister is the phase a newly registered version starts in; the
	// zero value means Pending.
	OnRegister Phase

	failImage  string
	failRevert bool
}

// FailImage scripts a bad release: a registration whose image contains
// substr (a version tag or a digest) starts in the Failed phase. With
// revert the fake then does what Nomad's auto_revert does: it registers the
// job's previous document again as a new job version, healthy. Without it
// the failed version stays the job's current one, for a caller that has to
// roll back itself. An empty substr clears the script.
func (f *Fake) FailImage(substr string, revert bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failImage, f.failRevert = substr, revert
}

// New starts a fake that accepts token in namespace.
func New(t *testing.T, token, namespace string) *Fake {
	t.Helper()
	f := &Fake{token: token, namespace: namespace, jobs: map[string]*job{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the fake's origin.
func (f *Fake) URL() string { return f.srv.URL }

// Requests returns a copy of every request so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Count returns the number of requests with this method whose path starts
// with prefix.
func (f *Fake) Count(method, prefix string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

// Override replaces the answer for matching requests; return ok=false to
// fall through.
func (f *Fake) Override(fn func(r Request) (status int, body string, ok bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override = fn
}

// SetPhase scripts the allocation of the job's current version.
func (f *Fake) SetPhase(jobID string, p Phase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		j.phase = p
	}
}

// Has reports whether the job exists, and whether it is stopped.
func (f *Fake) Has(jobID string) (exists, stopped bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[jobID]
	if j == nil {
		return false, false
	}
	return true, j.stop
}

// Version is the job's current version, 0 when absent.
func (f *Fake) Version(jobID string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		return j.version
	}
	return 0
}

// Job returns the last registered job document.
func (f *Fake) Job(jobID string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		return j.raw
	}
	return nil
}

// Restarts is how many times the job's allocation was restarted.
func (f *Fake) Restarts(jobID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		return j.restarts
	}
	return 0
}

// Image is the image of the registered job, empty when absent.
func (f *Fake) Image(jobID string) string {
	return imageOf(f.Job(jobID))
}

func imageOf(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	groups, _ := raw["TaskGroups"].([]any)
	if len(groups) == 0 {
		return ""
	}
	tasks, _ := groups[0].(map[string]any)["Tasks"].([]any)
	if len(tasks) == 0 {
		return ""
	}
	cfg, _ := tasks[0].(map[string]any)["Config"].(map[string]any)
	s, _ := cfg["image"].(string)
	return s
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Token: r.Header.Get("X-Nomad-Token"), Body: string(body)}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	ov := f.override
	f.mu.Unlock()
	// The override runs unlocked, so it may block, or call the fake's own
	// methods.
	if ov != nil {
		if st, b, ok := ov(req); ok {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(b))
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Token != f.token {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	if r.URL.Query().Get("namespace") != f.namespace {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
		f.register(w, body)
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "job":
		f.jobRoute(w, r, parts[2])
	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "job" && parts[3] == "allocations" && r.Method == http.MethodGet:
		f.allocations(w, parts[2])
	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "job" && parts[3] == "deployment" && r.Method == http.MethodGet:
		f.deployment(w, parts[2])
	case len(parts) == 5 && parts[1] == "client" && parts[2] == "allocation" && parts[4] == "restart" && r.Method == http.MethodPut:
		f.restart(w, parts[3])
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) register(w http.ResponseWriter, body []byte) {
	var p struct {
		Job            map[string]any `json:"Job"`
		EnforceIndex   bool           `json:"EnforceIndex"`
		JobModifyIndex uint64         `json:"JobModifyIndex"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Job == nil {
		http.Error(w, "bad job", http.StatusBadRequest)
		return
	}
	id, _ := p.Job["ID"].(string)
	ns, _ := p.Job["Namespace"].(string)
	if id == "" || ns != f.namespace {
		http.Error(w, "job namespace does not match", http.StatusBadRequest)
		return
	}
	cur := f.jobs[id]
	if p.EnforceIndex {
		switch {
		case p.JobModifyIndex == 0 && cur != nil:
			http.Error(w, fmt.Sprintf("Enforcing job modify index %d: job already exists", p.JobModifyIndex), http.StatusInternalServerError)
			return
		case p.JobModifyIndex != 0 && (cur == nil || cur.modifyIndex != p.JobModifyIndex):
			http.Error(w, fmt.Sprintf("Enforcing job modify index %d: job exists with conflicting job modify index", p.JobModifyIndex), http.StatusInternalServerError)
			return
		}
	}
	spec := string(mustJSON(p.Job))
	f.index++
	if cur == nil {
		cur = &job{phase: Pending}
		f.jobs[id] = cur
	}
	if cur.spec != spec || cur.stop {
		if cur.allocID != "" && !cur.stop {
			cur.old = append(cur.old, map[string]any{
				"ID": cur.allocID, "JobVersion": cur.version, "DesiredStatus": "run", "ClientStatus": "running",
				"DeploymentStatus": map[string]any{"Healthy": true},
			})
		}
		cur.version++
		cur.phase = Pending
		if f.OnRegister != "" {
			cur.phase = f.OnRegister
		}
		cur.polls = 0
		cur.allocSeq++
		cur.allocID = fmt.Sprintf("a110c%03d-0000-0000-0000-%012d", cur.allocSeq, len(id))
	}
	prevRaw, prevSpec := cur.raw, cur.spec
	cur.spec, cur.raw, cur.stop, cur.modifyIndex = spec, p.Job, false, f.index
	if f.failImage != "" && strings.Contains(imageOf(p.Job), f.failImage) {
		cur.phase = Failed
		if f.failRevert && prevRaw != nil && prevSpec != spec {
			// The failed deployment's allocation is replaced by one of the
			// previous document, registered as a new job version.
			cur.version++
			cur.allocSeq++
			cur.allocID = fmt.Sprintf("a110c%03d-0000-0000-0000-%012d", cur.allocSeq, len(id))
			cur.spec, cur.raw, cur.phase, cur.polls = prevSpec, prevRaw, Healthy, 0
		}
	}
	writeJSON(w, map[string]any{"EvalID": "ev1", "JobModifyIndex": cur.modifyIndex, "Warnings": ""})
}

func (f *Fake) jobRoute(w http.ResponseWriter, r *http.Request, id string) {
	j := f.jobs[id]
	switch r.Method {
	case http.MethodGet:
		if j == nil {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		doc := map[string]any{}
		for k, v := range j.raw {
			doc[k] = v
		}
		status := "running"
		if j.stop {
			status = "dead"
		}
		doc["Status"], doc["Stop"], doc["Version"], doc["JobModifyIndex"] = status, j.stop, j.version, j.modifyIndex
		if j.phase == Failed && !j.stop {
			doc["Status"] = "pending"
		}
		writeJSON(w, doc)
	case http.MethodDelete:
		if j == nil {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("purge") == "true" {
			delete(f.jobs, id)
		} else {
			j.stop = true
			f.index++
			j.modifyIndex = f.index
		}
		writeJSON(w, map[string]any{"EvalID": "ev2"})
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) allocations(w http.ResponseWriter, id string) {
	j := f.jobs[id]
	if j == nil {
		writeJSON(w, []any{})
		return
	}
	j.polls++
	if j.phase == Pending && f.HealthyAfter > 0 && j.polls >= f.HealthyAfter {
		j.phase = Healthy
	}
	a := map[string]any{"ID": j.allocID, "JobVersion": j.version, "DesiredStatus": "run", "ClientStatus": "pending"}
	switch {
	case j.stop:
		a["DesiredStatus"], a["ClientStatus"] = "stop", "complete"
	case j.phase == Healthy || j.phase == Unhealthy:
		a["ClientStatus"] = "running"
		a["DeploymentStatus"] = map[string]any{"Healthy": j.phase == Healthy}
		a["AllocatedResources"] = map[string]any{"Shared": map[string]any{"Ports": []any{
			map[string]any{"Label": "http", "Value": 23456, "To": 8080, "HostIP": "10.0.0.7"},
		}}}
	case j.phase == Failed:
		a["ClientStatus"] = "failed"
	}
	out := []any{}
	for _, o := range j.old {
		out = append(out, o)
	}
	writeJSON(w, append(out, a))
}

func (f *Fake) deployment(w http.ResponseWriter, id string) {
	j := f.jobs[id]
	if j == nil {
		writeJSON(w, nil)
		return
	}
	status := "running"
	switch j.phase {
	case Healthy:
		status = "successful"
	case Failed:
		status = "failed"
	}
	writeJSON(w, map[string]any{"ID": "dep1", "Status": status, "JobVersion": j.version})
}

func (f *Fake) restart(w http.ResponseWriter, alloc string) {
	for _, j := range f.jobs {
		if j.allocID == alloc {
			j.restarts++
			writeJSON(w, map[string]any{})
			return
		}
	}
	http.Error(w, "alloc not found", http.StatusNotFound)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
