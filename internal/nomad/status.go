package nomad

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

// JobInfo is what the control plane reads of a registered job.
type JobInfo struct {
	ID          string
	Status      string // pending, running or dead
	Stop        bool
	Version     uint64
	ModifyIndex uint64
	Meta        map[string]string
	// Image is the docker image of the first task, empty when unreadable.
	Image string
}

// Alloc is one allocation of a job.
type Alloc struct {
	ID            string
	JobVersion    uint64
	ClientStatus  string // pending, running, complete, failed or lost
	DesiredStatus string // run, stop or evict
	// Healthy is the deployment's verdict on the allocation; nil until Nomad
	// has decided.
	Healthy *bool
	// Address is host:port of the allocation's "http" port, empty until the
	// allocation has one. This is where the proxy connects.
	Address string
	// Restarts is the sum of the tasks' restart counts. It only grows for
	// one allocation, so a rise means the task was restarted.
	Restarts int
}

// Deployment is the latest deployment of a job.
type Deployment struct {
	ID         string
	Status     string // running, successful, failed, cancelled, ...
	JobVersion uint64
}

type wireJob struct {
	ID             string            `json:"ID"`
	Status         string            `json:"Status"`
	Stop           bool              `json:"Stop"`
	Version        uint64            `json:"Version"`
	JobModifyIndex uint64            `json:"JobModifyIndex"`
	Meta           map[string]string `json:"Meta"`
	TaskGroups     []struct {
		Tasks []struct {
			Config map[string]any `json:"Config"`
		} `json:"Tasks"`
	} `json:"TaskGroups"`
}

// Job reads a job. ErrNotFound when Nomad has none.
func (c *Client) Job(ctx context.Context, jobID string) (*JobInfo, error) {
	if err := c.check(jobID); err != nil {
		return nil, err
	}
	var w wireJob
	if err := c.do(ctx, request{op: "job", method: http.MethodGet, path: "/v1/job/" + url.PathEscape(jobID)}, &w); err != nil {
		return nil, err
	}
	if w.ID != jobID {
		return nil, &Error{Op: "job", Err: ErrProtocol}
	}
	j := &JobInfo{ID: w.ID, Status: w.Status, Stop: w.Stop, Version: w.Version, ModifyIndex: w.JobModifyIndex, Meta: w.Meta}
	if len(w.TaskGroups) > 0 && len(w.TaskGroups[0].Tasks) > 0 {
		j.Image, _ = w.TaskGroups[0].Tasks[0].Config["image"].(string)
	}
	return j, nil
}

type wireAlloc struct {
	ID               string `json:"ID"`
	JobVersion       uint64 `json:"JobVersion"`
	ClientStatus     string `json:"ClientStatus"`
	DesiredStatus    string `json:"DesiredStatus"`
	DeploymentStatus *struct {
		Healthy *bool `json:"Healthy"`
	} `json:"DeploymentStatus"`
	TaskStates map[string]struct {
		Restarts int `json:"Restarts"`
	} `json:"TaskStates"`
	AllocatedResources *struct {
		Shared struct {
			Ports []struct {
				Label  string `json:"Label"`
				Value  int    `json:"Value"`
				HostIP string `json:"HostIP"`
			} `json:"Ports"`
		} `json:"Shared"`
	} `json:"AllocatedResources"`
}

// Allocations lists the job's allocations.
func (c *Client) Allocations(ctx context.Context, jobID string) ([]Alloc, error) {
	if err := c.check(jobID); err != nil {
		return nil, err
	}
	var ws []wireAlloc
	if err := c.do(ctx, request{op: "allocations", method: http.MethodGet, path: "/v1/job/" + url.PathEscape(jobID) + "/allocations"}, &ws); err != nil {
		return nil, err
	}
	out := make([]Alloc, 0, len(ws))
	for _, w := range ws {
		a := Alloc{ID: w.ID, JobVersion: w.JobVersion, ClientStatus: w.ClientStatus, DesiredStatus: w.DesiredStatus}
		if w.DeploymentStatus != nil {
			a.Healthy = w.DeploymentStatus.Healthy
		}
		for _, ts := range w.TaskStates {
			a.Restarts += ts.Restarts
		}
		if w.AllocatedResources != nil {
			for _, p := range w.AllocatedResources.Shared.Ports {
				if p.Label == PortLabel && p.HostIP != "" && p.Value > 0 {
					a.Address = net.JoinHostPort(p.HostIP, strconv.Itoa(p.Value))
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// LatestDeployment reads the job's most recent deployment, nil when it has
// none.
func (c *Client) LatestDeployment(ctx context.Context, jobID string) (*Deployment, error) {
	if err := c.check(jobID); err != nil {
		return nil, err
	}
	var w *struct {
		ID         string `json:"ID"`
		Status     string `json:"Status"`
		JobVersion uint64 `json:"JobVersion"`
	}
	err := c.do(ctx, request{op: "deployment", method: http.MethodGet, path: "/v1/job/" + url.PathEscape(jobID) + "/deployment"}, &w)
	if err != nil || w == nil {
		return nil, err
	}
	return &Deployment{ID: w.ID, Status: w.Status, JobVersion: w.JobVersion}, nil
}

// Phase is the control plane's reading of a tenant job.
type Phase string

const (
	// PhaseAbsent: Nomad has no such job.
	PhaseAbsent Phase = "absent"
	// PhasePending: registered, not (yet) healthy. This includes an
	// allocation that is being restarted or rescheduled.
	PhasePending Phase = "pending"
	// PhaseHealthy: the current version has a running, healthy allocation.
	PhaseHealthy Phase = "healthy"
	// PhaseUnhealthy: the current version's allocation runs but Nomad judged
	// it unhealthy.
	PhaseUnhealthy Phase = "unhealthy"
	// PhaseFailed: Nomad gave up: the deployment failed or the job is dead.
	PhaseFailed Phase = "failed"
	// PhaseStopped: the job was stopped.
	PhaseStopped Phase = "stopped"
)

// Status is a tenant job's phase with what the proxy and the updater need.
type Status struct {
	Phase Phase
	Job   *JobInfo
	// AllocID and Address are those of the running allocation of the
	// current version, empty otherwise.
	AllocID string
	Address string
	// Restarts is the restart count of that allocation.
	Restarts int
	// Checks and ChecksRead are the live check results (StatusLive only).
	Checks     []LiveCheck
	ChecksRead bool
}

// Status reads the job, its allocations and its latest deployment and
// reduces them to a Phase. Only allocations of the job's current version
// count: a rolled-back or superseded allocation says nothing about it.
//
// Health here is Nomad's deployment verdict on the /healthz check and the
// client status of the allocation. Nomad decides that verdict once, after
// min_healthy_time, and never changes it, so it says nothing about the
// instance later; StatusLive adds the live check results.
func (c *Client) Status(ctx context.Context, jobID string) (Status, error) {
	job, err := c.Job(ctx, jobID)
	if err != nil {
		if isNotFound(err) {
			return Status{Phase: PhaseAbsent}, nil
		}
		return Status{}, err
	}
	st := Status{Job: job}
	if job.Stop {
		st.Phase = PhaseStopped
		return st, nil
	}
	allocs, err := c.Allocations(ctx, jobID)
	if err != nil {
		return Status{}, err
	}
	for _, a := range allocs {
		if a.JobVersion != job.Version || a.DesiredStatus != "run" || a.ClientStatus != "running" {
			continue
		}
		st.AllocID, st.Address, st.Restarts = a.ID, a.Address, a.Restarts
		switch {
		case a.Healthy != nil && *a.Healthy:
			st.Phase = PhaseHealthy
			return st, nil
		case a.Healthy != nil:
			st.Phase = PhaseUnhealthy
			return st, nil
		}
	}
	dep, err := c.LatestDeployment(ctx, jobID)
	if err != nil {
		return Status{}, err
	}
	switch {
	case dep != nil && dep.JobVersion == job.Version && dep.Status == "failed":
		st.Phase = PhaseFailed
	case job.Status == "dead":
		st.Phase = PhaseFailed
	default:
		st.Phase = PhasePending
	}
	return st, nil
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// CheckStatus is the state of one service check on an allocation.
type CheckStatus string

const (
	CheckSuccess CheckStatus = "success"
	CheckFailure CheckStatus = "failure"
	CheckPending CheckStatus = "pending"
)

// LiveCheck is a live service check result.
type LiveCheck struct {
	Name   string
	Status CheckStatus
}

// Checks reads the live results of the Nomad-provider service checks of an
// allocation (GET /v1/allocation/<id>/checks). Verified against a Nomad
// 2.0.6 agent: the answer is a map from check id to {Check, Service, Status,
// Output, StatusCode, Timestamp, Mode}, Status being "success" for a passing
// check. "failure" and "pending" are taken from Nomad's documentation (a
// failing check could not be produced against the dev agent).
func (c *Client) Checks(ctx context.Context, allocID string) ([]LiveCheck, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if !allocIDRE.MatchString(allocID) {
		return nil, &Error{Op: "checks", Err: ErrInvalidJob}
	}
	var m map[string]struct {
		Check  string `json:"Check"`
		Status string `json:"Status"`
	}
	if err := c.do(ctx, request{op: "checks", method: http.MethodGet, path: "/v1/allocation/" + allocID + "/checks"}, &m); err != nil {
		return nil, err
	}
	out := make([]LiveCheck, 0, len(m))
	for _, v := range m {
		out = append(out, LiveCheck{Name: v.Check, Status: CheckStatus(v.Status)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// StatusLive is Status plus the live check results of the running
// allocation: a healthy phase with a failing check becomes PhaseUnhealthy,
// because the deployment verdict, once healthy, never changes back. Checks is
// set when they were read. When the checks cannot be read the plain status
// stands.
func (c *Client) StatusLive(ctx context.Context, jobID string) (Status, error) {
	st, err := c.Status(ctx, jobID)
	if err != nil || st.AllocID == "" || (st.Phase != PhaseHealthy && st.Phase != PhaseUnhealthy) {
		return st, err
	}
	checks, cerr := c.Checks(ctx, st.AllocID)
	if cerr != nil {
		return st, nil
	}
	st.Checks, st.ChecksRead = checks, true
	for _, ch := range checks {
		if ch.Status == CheckFailure {
			st.Phase = PhaseUnhealthy
		}
	}
	return st, nil
}
