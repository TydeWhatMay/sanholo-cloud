package nomad_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
)

var update = flag.Bool("update", false, "regenerate testdata/job.golden.json")

func render(t *testing.T, s nomad.JobSettings, tenantID, version string) []byte {
	t.Helper()
	job, err := nomad.BuildJob(s, tenantID, version)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(map[string]any{"Job": job}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func TestJobGolden(t *testing.T) {
	got := render(t, nomadfake.Settings(), "acme-42", "0.7.1")
	const path = "testdata/job.golden.json"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("rendered job differs from %s (run go test ./internal/nomad -update to regenerate and review the diff)\n%s", path, got)
	}
}

func TestJobDeterministic(t *testing.T) {
	a := render(t, nomadfake.Settings(), "acme-42", "0.7.1")
	b := render(t, nomadfake.Settings(), "acme-42", "v0.7.1")
	if !bytes.Equal(a, b) {
		t.Error("the same inputs rendered two different jobs (v prefix must not matter)")
	}
}

// jobOf decodes the rendered job generically so the tests read it the way
// Nomad does.
func jobOf(t *testing.T, s nomad.JobSettings, tenantID string) map[string]any {
	t.Helper()
	var doc struct{ Job map[string]any }
	if err := json.Unmarshal(render(t, s, tenantID, "0.7.1"), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Job
}

func firstTask(job map[string]any) map[string]any {
	g := job["TaskGroups"].([]any)[0].(map[string]any)
	return g["Tasks"].([]any)[0].(map[string]any)
}

func group(job map[string]any) map[string]any { return job["TaskGroups"].([]any)[0].(map[string]any) }

func TestJobShape(t *testing.T) {
	job := jobOf(t, nomadfake.Settings(), "acme-42")
	if job["ID"] != "sanholo-tenant-acme-42" || job["Namespace"] != "sanholo" || job["Type"] != "service" {
		t.Errorf("identity of the job: %v %v %v", job["ID"], job["Namespace"], job["Type"])
	}
	groups := job["TaskGroups"].([]any)
	if len(groups) != 1 || group(job)["Count"].(float64) != 1 {
		t.Fatalf("want one group of count 1, got %d", len(groups))
	}
	tasks := group(job)["Tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("want one task, got %d", len(tasks))
	}
	task := firstTask(job)
	if task["Driver"] != "docker" {
		t.Errorf("driver = %v", task["Driver"])
	}
	cfg := task["Config"].(map[string]any)
	if cfg["image"] != "ghcr.io/tydewhatmay/sanholo:v0.7.1" {
		t.Errorf("image = %v", cfg["image"])
	}
	if got := time.Duration(task["KillTimeout"].(float64)); got != 20*time.Second {
		t.Errorf("kill_timeout = %v, want 20s (the client drains within it)", got)
	}
	res := task["Resources"].(map[string]any)
	if res["MemoryMB"].(float64) != 384 || res["CPU"].(float64) != 300 {
		t.Errorf("resources = %v", res)
	}
}

// Nomad writes a file identity to nomad_<name>.jwt and the client reads
// nomad_qm_sanholo.jwt and nomad_sanholo_cloud.jwt. Nomad refuses identity
// names that do not match ^[a-zA-Z0-9-_]{1,128}$ (so no dots), while the
// audiences keep theirs: qm's role and the alert verifier match
// "qm.sanholo" and "sanholo.cloud".
func TestJobIdentities(t *testing.T) {
	task := firstTask(jobOf(t, nomadfake.Settings(), "acme-42"))
	want := map[string]string{"qm_sanholo": "qm.sanholo", "sanholo_cloud": "sanholo.cloud"}
	got := map[string]string{}
	for _, raw := range task["Identities"].([]any) {
		id := raw.(map[string]any)
		aud := id["Audience"].([]any)
		if len(aud) != 1 || id["File"] != true {
			t.Errorf("identity %v: audience %v file %v", id["Name"], aud, id["File"])
		}
		got[id["Name"].(string)] = aud[0].(string)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identities = %v, want %v", got, want)
	}
	args := strings.Join(toStrings(task["Config"].(map[string]any)["args"]), " ")
	for _, f := range []string{"${NOMAD_SECRETS_DIR}/nomad_qm_sanholo.jwt", "${NOMAD_SECRETS_DIR}/nomad_sanholo_cloud.jwt"} {
		if !strings.Contains(args, f) {
			t.Errorf("args do not read %s", f)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The arguments are exactly the client's hosted flags.
func TestJobArgs(t *testing.T) {
	task := firstTask(jobOf(t, nomadfake.Settings(), "acme-42"))
	args := toStrings(task["Config"].(map[string]any)["args"])
	want := []string{
		"serve", "-hosted", "-live",
		"-addr", "0.0.0.0:8080", "-hosted-allow-any-bind",
		"-hosted-tenant", "acme-42",
		"-hosted-proxy-pubkey", "${NOMAD_TASK_DIR}/proxy-pubkey.pem",
		"-credentials-source", "qm",
		"-qm-addr", "https://qm.example.org:8200",
		"-qm-auth-mount", "nomad",
		"-qm-role", "sanholo-tenant",
		"-qm-identity-file", "${NOMAD_SECRETS_DIR}/nomad_qm_sanholo.jwt",
		"-hosted-alert-url", "https://app.example.org/internal/alerts",
		"-hosted-alert-identity-file", "${NOMAD_SECRETS_DIR}/nomad_sanholo_cloud.jwt",
		"-public-url", "https://acme-42.sanholo.space",
		"-catalog", "/data/catalog.db",
		"-hosted-vapid-public-key", nomadfake.VAPIDKey(),
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
	s := nomadfake.Settings()
	s.VAPIDPublicKey = ""
	noVapid := toStrings(firstTask(jobOf(t, s, "acme-42"))["Config"].(map[string]any)["args"])
	if strings.Contains(strings.Join(noVapid, " "), "vapid") {
		t.Error("the VAPID flag is passed without a key")
	}
}

func TestJobHealthAndPolicies(t *testing.T) {
	g := group(jobOf(t, nomadfake.Settings(), "acme-42"))
	svc := g["Services"].([]any)[0].(map[string]any)
	chk := svc["Checks"].([]any)[0].(map[string]any)
	if chk["Type"] != "http" || chk["Path"] != "/healthz" || chk["PortLabel"] != "http" {
		t.Errorf("check = %v", chk)
	}
	cr := chk["CheckRestart"].(map[string]any)
	sustained := time.Duration(cr["Limit"].(float64)) * time.Duration(chk["Interval"].(float64))
	if sustained < 2*time.Minute {
		t.Errorf("a restart follows %v of failures; the client reports a stall only after 2 minutes", sustained)
	}
	if time.Duration(cr["Grace"].(float64)) < time.Minute {
		t.Errorf("check_restart grace = %v", cr["Grace"])
	}
	up := g["Update"].(map[string]any)
	if up["HealthCheck"] != "checks" || up["AutoRevert"] != true || up["MaxParallel"].(float64) != 1 {
		t.Errorf("update = %v", up)
	}
	rp := g["RestartPolicy"].(map[string]any)
	if rp["Attempts"].(float64) != 3 || time.Duration(rp["Interval"].(float64)) != 10*time.Minute || rp["Mode"] != "fail" {
		t.Errorf("restart = %v", rp)
	}
	if g["ReschedulePolicy"].(map[string]any)["Unlimited"] != true {
		t.Error("a long-running bot must be rescheduled without limit")
	}
}

func TestJobNetworkAndVolume(t *testing.T) {
	job := jobOf(t, nomadfake.Settings(), "acme-42")
	g := group(job)
	net := g["Networks"].([]any)[0].(map[string]any)
	port := net["DynamicPorts"].([]any)[0].(map[string]any)
	if net["Mode"] != "bridge" || port["Label"] != "http" || port["To"].(float64) != 8080 || port["HostNetwork"] != "private" {
		t.Errorf("network = %v", net)
	}
	vol := g["Volumes"].(map[string]any)["data"].(map[string]any)
	if vol["Type"] != "host" || vol["Source"] != "sanholo-tenant-acme-42" || vol["ReadOnly"] != false {
		t.Errorf("volume = %v", vol)
	}
	m := firstTask(job)["VolumeMounts"].([]any)[0].(map[string]any)
	if m["Volume"] != "data" || m["Destination"] != "/data" {
		t.Errorf("mount = %v", m)
	}
}

func TestBuildJobRefusesBadTenant(t *testing.T) {
	for _, id := range []string{"", "Acme", "-a", "a-", "a/b", "a.b", "..", "a b", "a*", strings.Repeat("a", 64), "a\nb"} {
		if _, err := nomad.BuildJob(nomadfake.Settings(), id, "0.7.1"); err == nil {
			t.Errorf("tenant id %q accepted", id)
		}
	}
	for _, v := range []string{"", "latest", "0.7", "1.2.3.4", "0.7.1;rm", "v", "0.7.1 "} {
		if v == "0.7.1 " { // surrounding space is trimmed
			continue
		}
		if _, err := nomad.BuildJob(nomadfake.Settings(), "acme", v); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
}

func TestSpecHashTracksTheJob(t *testing.T) {
	spec := func(s nomad.JobSettings, id, v string) string {
		job, err := nomad.BuildJob(s, id, v)
		if err != nil {
			t.Fatal(err)
		}
		return job.Meta[nomad.MetaSpec]
	}
	base := spec(nomadfake.Settings(), "acme", "0.7.1")
	if base == "" || base != spec(nomadfake.Settings(), "acme", "0.7.1") {
		t.Fatal("the spec hash is empty or unstable")
	}
	bigger := nomadfake.Settings()
	bigger.MemoryMB = 512
	for name, other := range map[string]string{
		"version": spec(nomadfake.Settings(), "acme", "0.7.2"),
		"tenant":  spec(nomadfake.Settings(), "acme2", "0.7.1"),
		"memory":  spec(bigger, "acme", "0.7.1"),
	} {
		if other == base {
			t.Errorf("changing the %s left the spec hash unchanged", name)
		}
	}
}

func TestSettingsValidate(t *testing.T) {
	mod := func(f func(*nomad.JobSettings)) nomad.JobSettings {
		s := nomadfake.Settings()
		f(&s)
		return s
	}
	tests := []struct {
		name string
		s    nomad.JobSettings
		ok   bool
	}{
		{"valid", nomadfake.Settings(), true},
		{"valid no host network", mod(func(s *nomad.JobSettings) { s.HostNetwork = "" }), true},
		{"valid two keys", mod(func(s *nomad.JobSettings) { s.ProxyPublicKeys = nomadfake.ProxyKeyPEM(1) + nomadfake.ProxyKeyPEM(2) }), true},
		{"valid loopback http alert", mod(func(s *nomad.JobSettings) { s.AlertURL = "http://127.0.0.1:8090/internal/alerts" }), true},
		{"http qm", mod(func(s *nomad.JobSettings) { s.QMAddr = "http://qm.example.org" }), false},
		{"http alert", mod(func(s *nomad.JobSettings) { s.AlertURL = "http://app.example.org/internal/alerts" }), false},
		{"three keys", mod(func(s *nomad.JobSettings) {
			s.ProxyPublicKeys = nomadfake.ProxyKeyPEM(1) + nomadfake.ProxyKeyPEM(2) + nomadfake.ProxyKeyPEM(3)
		}), false},
		{"no key", mod(func(s *nomad.JobSettings) { s.ProxyPublicKeys = "" }), false},
		{"garbage key", mod(func(s *nomad.JobSettings) { s.ProxyPublicKeys = "not a key" }), false},
		{"template delimiters in key", mod(func(s *nomad.JobSettings) {
			s.ProxyPublicKeys = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=\n# {{ with secret \"x\" }}{{ end }}"
		}), false},
		{"raw key with a comment", mod(func(s *nomad.JobSettings) {
			s.ProxyPublicKeys = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=\n# rotated 2026-10"
		}), true},
		{"short vapid key", mod(func(s *nomad.JobSettings) { s.VAPIDPublicKey = "AAAA" }), false},
		{"bad repo", mod(func(s *nomad.JobSettings) { s.ImageRepo = "Bad Repo" }), false},
		{"repo with tag", mod(func(s *nomad.JobSettings) { s.ImageRepo = "ghcr.io/x/y:v1" }), false},
		{"no datacenter", mod(func(s *nomad.JobSettings) { s.Datacenters = nil }), false},
		{"bad domain", mod(func(s *nomad.JobSettings) { s.TenantDomain = "Sanholo Space" }), false},
		{"tiny memory", mod(func(s *nomad.JobSettings) { s.MemoryMB = 16 }), false},
		{"huge cpu", mod(func(s *nomad.JobSettings) { s.CPUMHz = 100000 }), false},
		{"bad host network", mod(func(s *nomad.JobSettings) { s.HostNetwork = "a b" }), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.s.Validate(); (err == nil) != tc.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// A job carries no secret: the settings have no field that could hold one,
// the environment is empty, and nothing in the rendered job looks like a
// credential once Nomad's own interpolation names are set aside.
func TestSettingsHaveNoSecretFields(t *testing.T) {
	bad := regexp.MustCompile(`(?i)token|secret|password|passwd|credential|apikey|bearer`)
	typ := reflect.TypeOf(nomad.JobSettings{})
	for i := range typ.NumField() {
		if n := typ.Field(i).Name; bad.MatchString(n) {
			t.Errorf("JobSettings.%s looks like a secret field", n)
		}
	}
}

func TestJobCarriesNoSecret(t *testing.T) {
	out := string(render(t, nomadfake.Settings(), "acme-42", "0.7.1"))
	if strings.Contains(out, `"Env"`) {
		t.Error("the job sets environment variables; the client needs none")
	}
	scrubbed := strings.NewReplacer("${NOMAD_SECRETS_DIR}", "", "-credentials-source", "", "nomad_qm_sanholo.jwt", "", "nomad_sanholo_cloud.jwt", "").Replace(out)
	for _, bad := range regexp.MustCompile(`(?i)token|secret|password|bearer|api_token|eyJ`).FindAllString(scrubbed, -1) {
		t.Errorf("the rendered job contains %q", bad)
	}
}

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestJobPinsTheImageDigest(t *testing.T) {
	s := nomadfake.Settings()
	plain, err := nomad.BuildJobPinned(s, "acme", "0.7.1", "")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := nomad.BuildJobPinned(s, "acme", "0.7.1", testDigest)
	if err != nil {
		t.Fatal(err)
	}
	image := func(j nomad.Job) any { return j.TaskGroups[0].Tasks[0].Config["image"] }
	if got := image(plain); got != "ghcr.io/tydewhatmay/sanholo:v0.7.1" {
		t.Errorf("unpinned image = %v", got)
	}
	if got := image(pinned); got != "ghcr.io/tydewhatmay/sanholo@"+testDigest {
		t.Errorf("pinned image = %v, want the repository at the digest", got)
	}
	if pinned.Meta[nomad.MetaVersion] != "0.7.1" {
		t.Errorf("the version left the meta: %v", pinned.Meta)
	}
	if pinned.Meta[nomad.MetaSpec] == plain.Meta[nomad.MetaSpec] {
		t.Error("a digest does not change the spec hash, so a pin would never be registered")
	}
	// BuildJob is the unpinned form.
	if j, _ := nomad.BuildJob(s, "acme", "0.7.1"); j.Meta[nomad.MetaSpec] != plain.Meta[nomad.MetaSpec] {
		t.Error("BuildJob and an empty digest differ")
	}
}

func TestJobRefusesABadDigest(t *testing.T) {
	for _, d := range []string{"sha256:abc", "md5:" + strings.Repeat("a", 64), testDigest + "0", strings.ToUpper(testDigest), "sha256:" + strings.Repeat("g", 64), " " + testDigest} {
		if _, err := nomad.BuildJobPinned(nomadfake.Settings(), "acme", "0.7.1", d); err == nil {
			t.Errorf("digest %q was accepted", d)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.7.1", "0.7.1", 0}, {"0.7.1", "0.8.0", -1}, {"0.8.0", "0.7.1", 1},
		{"0.9.0", "0.10.0", -1}, {"1.0.0", "0.99.99", 1},
		{"0.8.0-rc.1", "0.8.0", -1}, {"0.8.0", "0.8.0-rc.1", 1},
		{"0.8.0-rc.2", "0.8.0-rc.10", -1}, {"0.8.0-rc.1", "0.8.0-rc.1", 0},
		{"0.8.0-alpha", "0.8.0-beta", -1}, {"0.8.0-1", "0.8.0-alpha", -1},
		{"0.8.0-rc", "0.8.0-rc.1", -1},
		{"junk", "0.8.0", 0}, {"0.8.0", "1.0", 0},
	} {
		if got := nomad.CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// jobAt is jobOf at a given image version.
func jobAt(t *testing.T, s nomad.JobSettings, tenantID, version string) map[string]any {
	t.Helper()
	var doc struct{ Job map[string]any }
	if err := json.Unmarshal(render(t, s, tenantID, version), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Job
}

// servicesOf maps service name to service.
func servicesOf(job map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, raw := range group(job)["Services"].([]any) {
		svc := raw.(map[string]any)
		out[svc["Name"].(string)] = svc
	}
	return out
}

// portsOf maps port label to port.
func portsOf(job map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	net := group(job)["Networks"].([]any)[0].(map[string]any)
	for _, raw := range net["DynamicPorts"].([]any) {
		p := raw.(map[string]any)
		out[p["Label"].(string)] = p
	}
	return out
}

// From the client release that has -metrics-addr, the job maps a second
// port onto the same private host network, passes -metrics-addr for it, and
// registers it as its own service, so Prometheus scrapes it with no
// identity while the Bridge port and its service stay as they were.
func TestJobMetricsPort(t *testing.T) {
	job := jobAt(t, nomadfake.Settings(), "acme-42", nomad.MetricsSince)
	ports := portsOf(job)
	if len(ports) != 2 {
		t.Fatalf("ports = %v, want http and metrics", ports)
	}
	m := ports["metrics"]
	if m == nil || m["To"].(float64) != nomad.MetricsContainerPort || m["HostNetwork"] != "private" {
		t.Errorf("metrics port = %v, want To %d on host network private", m, nomad.MetricsContainerPort)
	}
	if h := ports["http"]; h == nil || h["To"].(float64) != 8080 || h["HostNetwork"] != "private" {
		t.Errorf("http port = %v", h)
	}

	args := toStrings(firstTask(job)["Config"].(map[string]any)["args"])
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-metrics-addr 0.0.0.0:9090") || !strings.Contains(joined, "-addr 0.0.0.0:8080 -hosted-allow-any-bind") {
		t.Errorf("args = %q, want -metrics-addr 0.0.0.0:9090 beside -addr 0.0.0.0:8080", args)
	}

	svcs := servicesOf(job)
	if len(svcs) != 2 {
		t.Fatalf("services = %v, want sanholo-bot and sanholo-bot-metrics", svcs)
	}
	ms := svcs["sanholo-bot-metrics"]
	if ms == nil || ms["PortLabel"] != "metrics" || ms["Provider"] != "nomad" || ms["AddressMode"] != "host" {
		t.Fatalf("metrics service = %v", ms)
	}
	if tags := toStrings(ms["Tags"]); !reflect.DeepEqual(tags, []string{"tenant=acme-42"}) {
		t.Errorf("metrics service tags = %v", tags)
	}
	// The deployment health stays the Bridge's /healthz alone.
	if chks, _ := ms["Checks"].([]any); len(chks) != 0 {
		t.Errorf("metrics service has checks %v; health is /healthz on http only", chks)
	}
	bot := svcs["sanholo-bot"]
	if bot == nil || bot["PortLabel"] != "http" {
		t.Errorf("sanholo-bot service = %v, want it on the http port", bot)
	}
}

// Releases before the flag existed must not get it: an unknown flag stops
// the client. Their job (and its spec hash) stays exactly as before.
func TestJobMetricsPortNeedsTheRelease(t *testing.T) {
	for _, v := range []string{"0.7.1", "0.7.9", "0.8.0-rc.1"} {
		job := jobAt(t, nomadfake.Settings(), "acme-42", v)
		if _, ok := portsOf(job)["metrics"]; ok {
			t.Errorf("%s: has a metrics port", v)
		}
		if _, ok := servicesOf(job)["sanholo-bot-metrics"]; ok {
			t.Errorf("%s: has a metrics service", v)
		}
		if args := strings.Join(toStrings(firstTask(job)["Config"].(map[string]any)["args"]), " "); strings.Contains(args, "-metrics-addr") {
			t.Errorf("%s: passes -metrics-addr", v)
		}
	}
	for _, v := range []string{"0.8.0", "0.8.1", "1.0.0"} {
		if _, ok := portsOf(jobAt(t, nomadfake.Settings(), "acme-42", v))["metrics"]; !ok {
			t.Errorf("%s: no metrics port", v)
		}
	}
}
