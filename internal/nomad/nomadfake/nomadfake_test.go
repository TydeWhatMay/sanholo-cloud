package nomadfake_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
)

const tok = "tok"

func register(t *testing.T, f *nomadfake.Fake, mut func(job map[string]any)) (int, string) {
	t.Helper()
	j, err := nomad.BuildJob(nomadfake.Settings(), "acme-42", "0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(j)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if mut != nil {
		mut(m)
	}
	body, _ := json.Marshal(map[string]any{"Job": m, "EnforceIndex": false})
	req, _ := http.NewRequest("POST", f.URL()+"/v1/jobs?namespace=sanholo", bytes.NewReader(body))
	req.Header.Set("X-Nomad-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func task(m map[string]any) map[string]any {
	return m["TaskGroups"].([]any)[0].(map[string]any)["Tasks"].([]any)[0].(map[string]any)
}

func grp(m map[string]any) map[string]any { return m["TaskGroups"].([]any)[0].(map[string]any) }

// The fake refuses what real Nomad refuses (each rule was checked against
// POST /v1/validate/job on Nomad 2.0.6), so a spec that real Nomad would
// reject cannot pass the tests.
func TestFakeRejectsWhatNomadRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(map[string]any)
		want string
	}{
		"identity with a dot": {func(m map[string]any) {
			task(m)["Identities"].([]any)[0].(map[string]any)["Name"] = "qm.sanholo"
		}, "identity name"},
		"identity with a space": {func(m map[string]any) {
			task(m)["Identities"].([]any)[0].(map[string]any)["Name"] = "qm sanholo"
		}, "identity name"},
		"identity too long": {func(m map[string]any) {
			task(m)["Identities"].([]any)[0].(map[string]any)["Name"] = strings.Repeat("a", 129)
		}, "identity name"},
		"service with an underscore": {func(m map[string]any) {
			grp(m)["Services"].([]any)[0].(map[string]any)["Name"] = "sanholo_bot"
		}, "Service name"},
		"service upper case": {func(m map[string]any) {
			grp(m)["Services"].([]any)[0].(map[string]any)["Name"] = "SanholoBot"
		}, "Service name"},
		"service too long": {func(m map[string]any) {
			grp(m)["Services"].([]any)[0].(map[string]any)["Name"] = strings.Repeat("a", 64)
		}, "Service name"},
		"service trailing dash": {func(m map[string]any) {
			grp(m)["Services"].([]any)[0].(map[string]any)["Name"] = "bot-"
		}, "Service name"},
		"job id with a space": {func(m map[string]any) { m["ID"] = "sanholo tenant" }, "space"},
		"task with a slash":   {func(m map[string]any) { task(m)["Name"] = "bo/t" }, "Task name"},
		"empty task":          {func(m map[string]any) { task(m)["Name"] = "" }, "Missing task name"},
		"empty group":         {func(m map[string]any) { grp(m)["Name"] = "" }, "task group name"},
	} {
		t.Run(name, func(t *testing.T) {
			f := nomadfake.New(t, tok, "sanholo")
			code, msg := register(t, f, tc.mut)
			if code != 400 || !strings.Contains(msg, tc.want) {
				t.Errorf("status %d %q, want 400 mentioning %q", code, msg, tc.want)
			}
		})
	}
	f := nomadfake.New(t, tok, "sanholo")
	if code, msg := register(t, f, nil); code != 200 {
		t.Errorf("the real tenant job was refused: %d %s", code, msg)
	}
}

func TestFakeLeavesAJobWithoutItsVolumeUnplaced(t *testing.T) {
	f := nomadfake.New(t, tok, "sanholo")
	if code, _ := register(t, f, nil); code != 200 {
		t.Fatal("registration refused")
	}
	req, _ := http.NewRequest("GET", f.URL()+"/v1/job/sanholo-tenant-acme-42/allocations?namespace=sanholo", nil)
	req.Header.Set("X-Nomad-Token", tok)
	resp, _ := http.DefaultClient.Do(req)
	var allocs []any
	_ = json.NewDecoder(resp.Body).Decode(&allocs)
	_ = resp.Body.Close()
	if len(allocs) != 0 {
		t.Errorf("an allocation was placed without the volume: %v", allocs)
	}
	f.AddVolume("sanholo-tenant-acme-42")
	resp, _ = http.DefaultClient.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&allocs)
	_ = resp.Body.Close()
	if len(allocs) != 1 {
		t.Errorf("with the volume there are %d allocations, want 1", len(allocs))
	}
}
