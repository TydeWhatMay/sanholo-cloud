package nomad

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
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
}

// Status reads the job, its allocations and its latest deployment and
// reduces them to a Phase. Only allocations of the job's current version
// count: a rolled-back or superseded allocation says nothing about it.
//
// Health is Nomad's own: the deployment's verdict on the /healthz check and
// the client status of the allocation. It is not a live poll of the check.
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
		st.AllocID, st.Address = a.ID, a.Address
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
