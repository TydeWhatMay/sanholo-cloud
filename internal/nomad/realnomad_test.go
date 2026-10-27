package nomad_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
)

// devAgent starts `nomad agent -dev` on free loopback ports with a data
// directory of its own, and always stops it (the whole process group) and
// removes the directory when the test ends. The test is skipped, with the
// reason, when no nomad binary is on PATH.
func devAgent(t *testing.T) (addr string) {
	t.Helper()
	bin, err := exec.LookPath("nomad")
	if err != nil {
		t.Skip("no nomad binary on PATH: the rendered job is not validated against a real Nomad here")
	}
	dir, err := os.MkdirTemp("", "sanholo-nomad-dev-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ports := make([]int, 3)
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
	}
	hostVols := filepath.Join(dir, "hostvols")
	if err := os.Mkdir(hostVols, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("ports {\n  http = %d\n  rpc  = %d\n  serf = %d\n}\nclient {\n  host_volumes_dir = %q\n}\n", ports[0], ports[1], ports[2], hostVols)
	cfgPath := filepath.Join(dir, "dev.hcl")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	cmd := exec.Command(bin, "agent", "-dev", "-bind=127.0.0.1", "-data-dir="+filepath.Join(dir, "data"), "-config="+cfgPath)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		// Any helper the agent left in its group goes too.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})

	addr = fmt.Sprintf("http://127.0.0.1:%d", ports[0])
	deadline := time.Now().Add(90 * time.Second)
	for {
		if ready(addr) {
			break
		}
		select {
		case <-done:
			t.Fatalf("the nomad dev agent exited early:\n%s", out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the nomad dev agent did not become ready:\n%s", out.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
	req, _ := http.NewRequest(http.MethodPut, addr+"/v1/namespace/sanholo", strings.NewReader(`{"Name":"sanholo"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("creating the namespace: %d", resp.StatusCode)
	}
	return addr
}

// ready: a leader, and a client node that is ready (volumes need a node).
func ready(addr string) bool {
	resp, err := http.Get(addr + "/v1/nodes")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	var nodes []struct{ Status string }
	if json.NewDecoder(resp.Body).Decode(&nodes) != nil {
		return false
	}
	return len(nodes) == 1 && nodes[0].Status == "ready"
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func validate(t *testing.T, addr string, job any) (errs []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Job": job})
	resp, err := http.Post(addr+"/v1/validate/job", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct{ ValidationErrors []string }
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("validate: %d %s", resp.StatusCode, raw)
	}
	return out.ValidationErrors
}

// The rendered job is checked by a real Nomad's own validation. The fake
// accepted names that real Nomad refuses (identity names with dots), so this
// is the test that does not share the fake's blind spots. It also runs the
// dynamic host volume calls against the real API.
func TestRenderedJobAndVolumeAgainstARealNomad(t *testing.T) {
	addr := devAgent(t)
	job, err := nomad.BuildJob(nomadfake.Settings(), "acme-42", "0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	if errs := validate(t, addr, job); len(errs) != 0 {
		t.Fatalf("Nomad rejects the rendered job:\n%s", strings.Join(errs, "\n"))
	}

	// Negative control: the same check does flag the identity names this
	// repository used to render, so a pass above means something.
	var raw map[string]any
	b, _ := json.Marshal(job)
	_ = json.Unmarshal(b, &raw)
	ids := raw["TaskGroups"].([]any)[0].(map[string]any)["Tasks"].([]any)[0].(map[string]any)["Identities"].([]any)
	ids[0].(map[string]any)["Name"] = "qm.sanholo"
	if errs := validate(t, addr, raw); len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "qm.sanholo") {
		t.Fatalf("a real Nomad did not flag a dotted identity name: %v", errs)
	}

	c, err := nomad.New(nomad.Config{Addr: addr, Token: "dev-agent-no-acl", Namespace: "sanholo", Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	vs := nomadfake.VolumeSettings()
	vs.Owner = "" // the dev agent does not run as root and cannot chown
	created, err := c.EnsureHostVolume(ctx, "acme-42", vs)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	created, err = c.EnsureHostVolume(ctx, "acme-42", vs)
	if err != nil || created {
		t.Fatalf("second create: %v %v (an existing volume must be reused)", created, err)
	}
	if ok, err := c.HostVolumeExists(ctx, "acme-42"); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	if err := c.DeleteHostVolume(ctx, "acme-42"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, _ := c.HostVolumeExists(ctx, "acme-42"); ok {
		t.Fatal("the volume is still there after the delete")
	}
	if err := c.DeleteHostVolume(ctx, "acme-42"); err != nil {
		t.Fatalf("deleting a gone volume: %v", err)
	}
}
