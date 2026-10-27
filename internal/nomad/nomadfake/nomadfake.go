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
//   - DELETE stops the job, with purge=true it forgets it;
//   - the deployment's health verdict is sticky, as in Nomad: decided once per
//     allocation, never changed. What changes later is modelled apart: live
//     service checks (SetCheck), task restarts (BumpRestarts) and a
//     replacement allocation (ReplaceAlloc);
//   - a registered job is validated with the rules of real Nomad that the
//     tenant job can break (identity, service, task and job names), checked
//     against a Nomad 2.0.6 dev agent (POST /v1/validate/job);
//   - dynamic host volumes (create, list by name, delete) with the shapes of
//     Nomad 2.0.6, and a job whose volume does not exist gets no allocation.
package nomadfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	// verdict is the deployment's health verdict on the current allocation,
	// set once.
	verdict Phase
	// check scripts the live check status ("" derives it from the phase).
	check string
	// taskRestarts is the task restart count of the current allocation.
	taskRestarts int
	// old are allocations of earlier versions, still listed (and healthy)
	// until the job is stopped, as Nomad keeps them while it replaces them.
	old []map[string]any
}

// Fake is the server and its state.
type Fake struct {
	t         *testing.T
	srv       *httptest.Server
	token     string
	namespace string

	mu       sync.Mutex
	volumes  map[string]*volume
	volSeq   int
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
	f := &Fake{t: t, token: token, namespace: namespace, jobs: map[string]*job{}, volumes: map[string]*volume{}}
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
		f.setPhase(j, p)
	}
}

// setPhase applies a scripted phase. Nomad decides an allocation's
// deployment health once; flipping a decided verdict is a test bug, so it
// fails the test instead of silently modelling something Nomad never does.
func (f *Fake) setPhase(j *job, p Phase) {
	if (p == Healthy || p == Unhealthy) && j.verdict != "" && j.verdict != p {
		f.t.Helper()
		f.t.Errorf("nomadfake: the deployment health of the allocation is already %q; Nomad decides it once. Model a later change with SetCheck, BumpRestarts or ReplaceAlloc", j.verdict)
		return
	}
	if p == Healthy || p == Unhealthy {
		j.verdict = p
	}
	if p == Pending || p == Failed {
		j.verdict = ""
	}
	j.phase = p
}

// SetCheck scripts the live status of the job's service check: "success",
// "failure" or "pending". An empty status derives it from the allocation.
func (f *Fake) SetCheck(jobID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		j.check = status
	}
}

// BumpRestarts adds a task restart to the current allocation, as Nomad's
// restart policy or check_restart would.
func (f *Fake) BumpRestarts(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		j.taskRestarts++
	}
}

// ReplaceAlloc replaces the job's allocation with a new one of the same job
// version, as a reschedule does: no deployment verdict yet, no restarts.
func (f *Fake) ReplaceAlloc(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[jobID]; j != nil {
		j.allocSeq++
		j.allocID = fmt.Sprintf("a110c%03d-0000-0000-0000-%012d", j.allocSeq, len(jobID))
		j.verdict, j.check, j.taskRestarts, j.phase, j.polls = "", "", 0, Pending, 0
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
	case len(parts) == 4 && parts[1] == "allocation" && parts[3] == "checks" && r.Method == http.MethodGet:
		f.checks(w, parts[2])
	case r.Method == http.MethodGet && r.URL.Path == "/v1/volumes":
		f.listVolumes(w, r)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/volume/host/create":
		f.createVolume(w, body)
	case r.Method == http.MethodDelete && len(parts) == 5 && parts[1] == "volume" && parts[2] == "host" && parts[4] == "delete":
		f.deleteVolume(w, parts[3])
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
	if msgs := validateJob(p.Job); len(msgs) > 0 {
		http.Error(w, strings.Join(msgs, "; "), http.StatusBadRequest)
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
		cur.phase, cur.verdict, cur.check, cur.taskRestarts = Pending, "", "", 0
		if f.OnRegister != "" {
			f.setPhase(cur, f.OnRegister)
		}
		cur.polls = 0
		cur.allocSeq++
		cur.allocID = fmt.Sprintf("a110c%03d-0000-0000-0000-%012d", cur.allocSeq, len(id))
	}
	prevRaw, prevSpec := cur.raw, cur.spec
	cur.spec, cur.raw, cur.stop, cur.modifyIndex = spec, p.Job, false, f.index
	if f.failImage != "" && strings.Contains(imageOf(p.Job), f.failImage) {
		cur.phase, cur.verdict = Failed, ""
		if f.failRevert && prevRaw != nil && prevSpec != spec {
			// The failed deployment's allocation is replaced by one of the
			// previous document, registered as a new job version.
			cur.version++
			cur.allocSeq++
			cur.allocID = fmt.Sprintf("a110c%03d-0000-0000-0000-%012d", cur.allocSeq, len(id))
			cur.spec, cur.raw, cur.phase, cur.polls = prevSpec, prevRaw, Healthy, 0
			cur.verdict, cur.check, cur.taskRestarts = Healthy, "", 0
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
		f.setPhase(j, Healthy)
	}
	if f.blocked(j) {
		writeJSON(w, []any{})
		return
	}
	a := map[string]any{"ID": j.allocID, "JobVersion": j.version, "DesiredStatus": "run", "ClientStatus": "pending"}
	a["TaskStates"] = map[string]any{"bot": map[string]any{"Restarts": j.taskRestarts, "State": "running"}}
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
			j.taskRestarts++ // Nomad counts a requested restart like any other (checked on 2.0.6)
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

// blocked: the job's volume does not exist, so Nomad cannot place the
// allocation and the evaluation stays blocked: no allocation is listed.
func (f *Fake) blocked(j *job) bool {
	if j.stop {
		return false
	}
	return f.missingVolume(j.raw) != ""
}

func (f *Fake) missingVolume(raw map[string]any) string {
	groups, _ := raw["TaskGroups"].([]any)
	for _, g := range groups {
		vols, _ := g.(map[string]any)["Volumes"].(map[string]any)
		for _, v := range vols {
			vm, _ := v.(map[string]any)
			if vm["Type"] == "host" {
				src, _ := vm["Source"].(string)
				if f.volumes[src] == nil {
					return src
				}
			}
		}
	}
	return ""
}

func (f *Fake) checks(w http.ResponseWriter, alloc string) {
	for _, j := range f.jobs {
		if j.allocID != alloc {
			continue
		}
		status := j.check
		if status == "" {
			status = "pending"
			if (j.phase == Healthy || j.phase == Unhealthy) && !j.stop {
				status = "success"
			}
		}
		writeJSON(w, map[string]any{"chk1": map[string]any{
			"Check": "healthz", "Service": "sanholo-bot", "Group": "bot", "Mode": "healthiness",
			"Status": status, "ID": "chk1", "Output": "nomad: http", "StatusCode": 200, "Timestamp": 1,
		}})
		return
	}
	http.Error(w, "Unknown allocation \""+alloc+"\"", http.StatusNotFound)
}

var (
	identityNameRE = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,128}$`)
	serviceNameRE  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)?$`)
)

// validateJob applies the rules of Nomad's job validation (verified against
// POST /v1/validate/job on Nomad 2.0.6) that a tenant job can break.
func validateJob(job map[string]any) []string {
	var out []string
	id, _ := job["ID"].(string)
	name, _ := job["Name"].(string)
	switch {
	case id == "":
		out = append(out, "Missing job ID")
	case strings.ContainsAny(id, " \t\r\n\x00"):
		out = append(out, "Job ID contains a space or a null character")
	}
	if name == "" {
		out = append(out, "Missing job name")
	}
	groups, _ := job["TaskGroups"].([]any)
	if len(groups) == 0 {
		out = append(out, "Missing job task groups")
	}
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		if n, _ := gm["Name"].(string); n == "" {
			out = append(out, "Missing task group name")
		}
		svcs, _ := gm["Services"].([]any)
		for _, sv := range svcs {
			n, _ := sv.(map[string]any)["Name"].(string)
			if !serviceNameRE.MatchString(n) {
				out = append(out, fmt.Sprintf("Service name %q must be a lower-case DNS label of at most 63 characters", n))
			}
		}
		tasks, _ := gm["Tasks"].([]any)
		for _, t := range tasks {
			tm, _ := t.(map[string]any)
			tn, _ := tm["Name"].(string)
			switch {
			case tn == "":
				out = append(out, "Missing task name")
			case strings.ContainsAny(tn, "/\\\x00"):
				out = append(out, "Task name cannot include slashes or null characters")
			}
			ids, _ := tm["Identities"].([]any)
			for _, i := range ids {
				in, _ := i.(map[string]any)["Name"].(string)
				if !identityNameRE.MatchString(in) {
					out = append(out, fmt.Sprintf("invalid identity name %q. Must match regex ^[a-zA-Z0-9-_]{1,128}$", in))
				}
			}
		}
	}
	return out
}

type volume struct {
	id, name string
	spec     map[string]any
}

// AddVolume creates a volume without a request (test setup).
func (f *Fake) AddVolume(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volSeq++
	f.volumes[name] = &volume{id: fmt.Sprintf("70100000-0000-0000-0000-%012d", f.volSeq), name: name, spec: map[string]any{}}
}

// RemoveVolume deletes a volume without a request (test setup).
func (f *Fake) RemoveVolume(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
}

// Volumes lists the names of the dynamic host volumes.
func (f *Fake) Volumes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.volumes {
		out = append(out, n)
	}
	return out
}

// VolumeSpec is the create request of a volume, nil when there is none.
func (f *Fake) VolumeSpec(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v := f.volumes[name]; v != nil {
		return v.spec
	}
	return nil
}

var volumeFilterRE = regexp.MustCompile(`^Name == "(.*)"$`)

func (f *Fake) listVolumes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("type") != "host" {
		http.Error(w, "volume type required", http.StatusBadRequest)
		return
	}
	want := ""
	if flt := r.URL.Query().Get("filter"); flt != "" {
		m := volumeFilterRE.FindStringSubmatch(flt)
		if m == nil {
			http.Error(w, "unsupported filter", http.StatusBadRequest)
			return
		}
		want = m[1]
	}
	out := []any{}
	for n, v := range f.volumes {
		if want == "" || n == want {
			out = append(out, map[string]any{"ID": v.id, "Name": n, "Namespace": f.namespace, "PluginID": "mkdir", "State": "ready"})
		}
	}
	writeJSON(w, out)
}

func (f *Fake) createVolume(w http.ResponseWriter, body []byte) {
	var p struct {
		Volume map[string]any `json:"Volume"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Volume == nil {
		http.Error(w, "missing volume", http.StatusBadRequest)
		return
	}
	v := p.Volume
	name, _ := v["Name"].(string)
	min, _ := v["RequestedCapacityMinBytes"].(float64)
	max, _ := v["RequestedCapacityMaxBytes"].(float64)
	caps, _ := v["RequestedCapabilities"].([]any)
	switch {
	case v["Namespace"] != f.namespace:
		http.Error(w, "volume namespace does not match", http.StatusBadRequest)
	case name == "" || v["PluginID"] != "mkdir":
		http.Error(w, "volume validation failed: name and plugin are required", http.StatusInternalServerError)
	case max < min || min <= 0:
		http.Error(w, fmt.Sprintf("volume validation failed: capacity_max (%d) must be larger than capacity_min (%d)", int64(max), int64(min)), http.StatusInternalServerError)
	case len(caps) == 0:
		http.Error(w, "volume validation failed: must include at least one capability block", http.StatusInternalServerError)
	case f.volumes[name] != nil:
		http.Error(w, fmt.Sprintf("could not place volume %q: no node meets constraints: 1 nodes had existing volume", name), http.StatusInternalServerError)
	default:
		f.volSeq++
		id := fmt.Sprintf("70100000-0000-0000-0000-%012d", f.volSeq)
		f.volumes[name] = &volume{id: id, name: name, spec: v}
		writeJSON(w, map[string]any{"Index": f.index, "Volume": map[string]any{"ID": id, "Name": name, "Namespace": f.namespace, "State": "pending"}, "Warnings": ""})
	}
}

func (f *Fake) deleteVolume(w http.ResponseWriter, id string) {
	for n, v := range f.volumes {
		if v.id != id {
			continue
		}
		for _, j := range f.jobs {
			if !j.stop && j.raw != nil && f.usesVolume(j.raw, n) {
				http.Error(w, "volume in use: an allocation still claims it", http.StatusInternalServerError)
				return
			}
		}
		delete(f.volumes, n)
		writeJSON(w, map[string]any{"Index": f.index})
		return
	}
	http.Error(w, "no such volume: "+id, http.StatusInternalServerError)
}

func (f *Fake) usesVolume(raw map[string]any, name string) bool {
	groups, _ := raw["TaskGroups"].([]any)
	for _, g := range groups {
		vols, _ := g.(map[string]any)["Volumes"].(map[string]any)
		for _, v := range vols {
			if vm, _ := v.(map[string]any); vm["Source"] == name {
				return true
			}
		}
	}
	return false
}
