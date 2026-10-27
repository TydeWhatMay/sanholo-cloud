package nomad

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lhaig/sanholo-cloud/internal/tenant"
)

// The tenant job. Everything the SanHolo client reads in -hosted mode is
// listed here and nowhere else; the flags and file names are the client's
// (cmd/san-holo/hosted.go, internal/qmcreds, internal/alert), verified
// against its source, not against the planning notes.
//
// Network assumptions (for the infra session, repeated in deploy/README.md):
//   - group network mode "bridge" (CNI): every tenant task has its own
//     network namespace, so listening on 0.0.0.0 inside it exposes nothing but
//     the namespace's own interface. The one port, "http", is mapped by Nomad
//     onto the host network named in JobSettings.HostNetwork (the private
//     interface) and nowhere else;
//   - only the control plane's proxy (and Prometheus) may connect to that
//     host port, and tenant tasks may connect out only to the game API, qm
//     and the control plane: both are firewall rules the infra session owns.
const (
	// PortLabel is the label of the one network port.
	PortLabel = "http"
	// ContainerPort is the port the bot listens on inside its namespace.
	ContainerPort = 8080
	// TaskName is the name of the one task.
	TaskName = "bot"
	// GroupName is the name of the one group.
	GroupName = "bot"
	// DataDir is where the host volume is mounted; the catalog lives there.
	DataDir = "/data"

	// QMIdentity is the name of the identity block for qm. Nomad writes the
	// JWT to ${NOMAD_SECRETS_DIR}/nomad_<name>.jwt and the client reads
	// nomad_qm.sanholo.jwt, so the name is the audience, dot included.
	QMIdentity = "qm.sanholo"
	// AlertIdentity is the name of the identity block for alerts; the client
	// reads nomad_sanholo.cloud.jwt.
	AlertIdentity = "sanholo.cloud"

	// QMRole and QMAuthMount name qm's tenant role and the jwt auth method
	// mounted at auth/nomad (qm contract, Appendix A.1). The client defaults
	// the mount to "jwt", so the job must pass it.
	QMRole      = "sanholo-tenant"
	QMAuthMount = "nomad"

	proxyKeyFile = "proxy-pubkey.pem"
	// MetaSpec is the job meta key holding the hash of the rendered job;
	// the same hash means nothing needs registering.
	MetaSpec = "sanholo.spec"
	// MetaVersion is the job meta key holding the image version.
	MetaVersion = "sanholo.version"
	// MetaTenant is the job meta key holding the tenant id.
	MetaTenant = "sanholo.tenant"
)

// DefaultImageRepo is the public image repository.
const DefaultImageRepo = "ghcr.io/tydewhatmay/sanholo"

// Timing of the job. Startup is about a second; warm-up can take minutes
// under the game's rate limit, but /healthz answers 200 "starting" for up to
// 10 minutes of it and only then 503 ("warm-up stuck"). After warm-up it
// answers 503 when the bot or poller is over 2 minutes stale. So: a restart
// follows only a sustained 503 (8 failures x 15 s = 2 minutes), never the
// warm-up itself.
const (
	killTimeout      = 20 * time.Second
	checkInterval    = 15 * time.Second
	checkTimeout     = 5 * time.Second
	checkRestartMax  = 8
	checkRestartWait = 2 * time.Minute
	minHealthyTime   = 30 * time.Second
	healthyDeadline  = 10 * time.Minute
	progressDeadline = 15 * time.Minute
	identityTTL      = time.Hour
)

// JobSettings are the deployment-wide values of a tenant job. There is no
// field for a secret: a job carries none (identities come from Nomad, the
// game token from qm), and TestSettingsHaveNoSecretFields keeps it so.
type JobSettings struct {
	Namespace   string
	Region      string
	Datacenters []string
	// ImageRepo is the image repository without a tag.
	ImageRepo string
	// HostNetwork names the Nomad host network the port is mapped onto (the
	// private interface). Empty means the node's default.
	HostNetwork string
	// QMAddr is qm's origin as the tenant reaches it.
	QMAddr string
	// AlertURL is the control plane's alert endpoint as the tenant reaches
	// it (https unless loopback).
	AlertURL string
	// VAPIDPublicKey is the Web Push public key (base64url, public); empty
	// leaves notifications off.
	VAPIDPublicKey string
	// ProxyPublicKeys is the PEM (or base64, one per line) Ed25519 public
	// key(s) the proxy signs with; two during a rotation.
	ProxyPublicKeys string
	// TenantDomain is the parent domain of the tenant subdomains.
	TenantDomain string
	MemoryMB     int
	CPUMHz       int
}

var (
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]{1,32})?$`)
	repoRE    = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
	domainRE  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	hostNetRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// NormalizeVersion accepts "0.7.1" or "v0.7.1" and returns "0.7.1".
func NormalizeVersion(v string) (string, error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !versionRE.MatchString(v) {
		return "", errors.New("version: want X.Y.Z, such as 0.7.1")
	}
	return v, nil
}

// Validate checks the settings. Errors never include a value that could be
// sensitive; the public key and URLs are public but are not echoed either.
func (s JobSettings) Validate() error {
	var errs []error
	if !namespaceRE.MatchString(s.Namespace) {
		errs = append(errs, errors.New("namespace: invalid"))
	}
	if len(s.Datacenters) == 0 {
		errs = append(errs, errors.New("datacenters: at least one"))
	}
	for _, d := range s.Datacenters {
		if !namespaceRE.MatchString(d) {
			errs = append(errs, errors.New("datacenters: invalid name"))
		}
	}
	if !repoRE.MatchString(s.ImageRepo) {
		errs = append(errs, errors.New("image repository: want a name such as ghcr.io/org/image"))
	}
	if s.HostNetwork != "" && !hostNetRE.MatchString(s.HostNetwork) {
		errs = append(errs, errors.New("host network: invalid name"))
	}
	if _, err := ParseAddr(s.QMAddr); err != nil {
		errs = append(errs, fmt.Errorf("qm address: %w", err))
	}
	if err := checkAlertURL(s.AlertURL); err != nil {
		errs = append(errs, fmt.Errorf("alert URL: %w", err))
	}
	if s.VAPIDPublicKey != "" {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s.VAPIDPublicKey, "="))
		if err != nil || len(raw) != 65 || raw[0] != 4 {
			errs = append(errs, errors.New("VAPID public key: want a base64url P-256 public key (65 bytes, uncompressed)"))
		}
	}
	if err := checkPublicKeys(s.ProxyPublicKeys); err != nil {
		errs = append(errs, fmt.Errorf("proxy public keys: %w", err))
	}
	if !domainRE.MatchString(s.TenantDomain) {
		errs = append(errs, errors.New("tenant domain: want a lower-case domain name"))
	}
	if s.MemoryMB < 128 || s.MemoryMB > 4096 {
		errs = append(errs, errors.New("memory: want 128 to 4096 MB"))
	}
	if s.CPUMHz < 50 || s.CPUMHz > 4000 {
		errs = append(errs, errors.New("cpu: want 50 to 4000 MHz"))
	}
	return errors.Join(errs...)
}

// checkAlertURL mirrors the client's rule: https unless loopback.
func checkAlertURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("not a URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return errors.New("http is allowed for loopback hosts only")
		}
	default:
		return errors.New("scheme must be https")
	}
	return nil
}

// checkPublicKeys applies the client's parser rules: PEM PKIX Ed25519
// blocks, or one base64 raw 32-byte key per line; one or two keys. The text
// also lands in a Nomad template, so it must hold no template delimiters.
func checkPublicKeys(text string) error {
	if len(text) > 4096 || strings.Contains(text, "{{") || strings.Contains(text, "}}") {
		return errors.New("too long or has template delimiters")
	}
	n := 0
	if strings.Contains(text, "-----BEGIN") {
		rest := []byte(text)
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
			if err != nil || blk.Type != "PUBLIC KEY" {
				return errors.New("not a PUBLIC KEY block")
			}
			if _, ok := pub.(ed25519.PublicKey); !ok {
				return errors.New("not an Ed25519 key")
			}
			n++
		}
		if len(strings.TrimSpace(string(rest))) > 0 {
			return errors.New("unparsable data after the last block")
		}
	} else {
		for line := range strings.SplitSeq(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			ok := false
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				if b, err := enc.DecodeString(line); err == nil && len(b) == ed25519.PublicKeySize {
					ok = true
					break
				}
			}
			if !ok {
				return errors.New("a line is not a base64 32-byte key")
			}
			n++
		}
	}
	if n < 1 || n > 2 {
		return errors.New("want one key, or two during a rotation")
	}
	return nil
}

// Job is the Nomad job in the API's JSON shape (PascalCase, durations in
// nanoseconds). Only the fields the template uses exist.
type Job struct {
	ID          string            `json:"ID"`
	Name        string            `json:"Name"`
	Namespace   string            `json:"Namespace"`
	Region      string            `json:"Region,omitempty"`
	Datacenters []string          `json:"Datacenters"`
	Type        string            `json:"Type"`
	Priority    int               `json:"Priority"`
	Meta        map[string]string `json:"Meta"`
	TaskGroups  []TaskGroup       `json:"TaskGroups"`
}

type TaskGroup struct {
	Name             string                   `json:"Name"`
	Count            int                      `json:"Count"`
	Networks         []Network                `json:"Networks"`
	Volumes          map[string]VolumeRequest `json:"Volumes"`
	RestartPolicy    RestartPolicy            `json:"RestartPolicy"`
	ReschedulePolicy ReschedulePolicy         `json:"ReschedulePolicy"`
	Update           Update                   `json:"Update"`
	Services         []Service                `json:"Services"`
	Tasks            []Task                   `json:"Tasks"`
}

type Network struct {
	Mode         string `json:"Mode"`
	DynamicPorts []Port `json:"DynamicPorts"`
}

type Port struct {
	Label       string `json:"Label"`
	To          int    `json:"To"`
	HostNetwork string `json:"HostNetwork,omitempty"`
}

type VolumeRequest struct {
	Name     string `json:"Name"`
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	ReadOnly bool   `json:"ReadOnly"`
}

type RestartPolicy struct {
	Attempts int           `json:"Attempts"`
	Interval time.Duration `json:"Interval"`
	Delay    time.Duration `json:"Delay"`
	Mode     string        `json:"Mode"`
}

type ReschedulePolicy struct {
	Delay         time.Duration `json:"Delay"`
	DelayFunction string        `json:"DelayFunction"`
	MaxDelay      time.Duration `json:"MaxDelay"`
	Unlimited     bool          `json:"Unlimited"`
}

type Update struct {
	MaxParallel      int           `json:"MaxParallel"`
	HealthCheck      string        `json:"HealthCheck"`
	MinHealthyTime   time.Duration `json:"MinHealthyTime"`
	HealthyDeadline  time.Duration `json:"HealthyDeadline"`
	ProgressDeadline time.Duration `json:"ProgressDeadline"`
	AutoRevert       bool          `json:"AutoRevert"`
	AutoPromote      bool          `json:"AutoPromote"`
}

type Service struct {
	Name        string   `json:"Name"`
	PortLabel   string   `json:"PortLabel"`
	Provider    string   `json:"Provider"`
	AddressMode string   `json:"AddressMode"`
	Tags        []string `json:"Tags"`
	Checks      []Check  `json:"Checks"`
}

type Check struct {
	Name         string        `json:"Name"`
	Type         string        `json:"Type"`
	Protocol     string        `json:"Protocol"`
	Method       string        `json:"Method"`
	Path         string        `json:"Path"`
	PortLabel    string        `json:"PortLabel"`
	Interval     time.Duration `json:"Interval"`
	Timeout      time.Duration `json:"Timeout"`
	CheckRestart CheckRestart  `json:"CheckRestart"`
}

type CheckRestart struct {
	Limit          int           `json:"Limit"`
	Grace          time.Duration `json:"Grace"`
	IgnoreWarnings bool          `json:"IgnoreWarnings"`
}

type Task struct {
	Name         string         `json:"Name"`
	Driver       string         `json:"Driver"`
	Config       map[string]any `json:"Config"`
	Identities   []Identity     `json:"Identities"`
	Templates    []Template     `json:"Templates"`
	VolumeMounts []VolumeMount  `json:"VolumeMounts"`
	Resources    Resources      `json:"Resources"`
	LogConfig    LogConfig      `json:"LogConfig"`
	KillTimeout  time.Duration  `json:"KillTimeout"`
	KillSignal   string         `json:"KillSignal"`
}

type Identity struct {
	Name     string        `json:"Name"`
	Audience []string      `json:"Audience"`
	File     bool          `json:"File"`
	TTL      time.Duration `json:"TTL"`
}

type Template struct {
	EmbeddedTmpl string `json:"EmbeddedTmpl"`
	DestPath     string `json:"DestPath"`
	ChangeMode   string `json:"ChangeMode"`
	Perms        string `json:"Perms"`
}

type VolumeMount struct {
	Volume      string `json:"Volume"`
	Destination string `json:"Destination"`
	ReadOnly    bool   `json:"ReadOnly"`
}

type Resources struct {
	CPU      int `json:"CPU"`
	MemoryMB int `json:"MemoryMB"`
}

type LogConfig struct {
	MaxFiles      int `json:"MaxFiles"`
	MaxFileSizeMB int `json:"MaxFileSizeMB"`
}

// VolumeName is the host volume of a tenant. The infra session creates one
// per tenant under this name (see deploy/README.md).
func VolumeName(tenantID string) string { return JobPrefix + tenantID }

// Image is the image reference of a version.
func (s JobSettings) Image(version string) string { return s.ImageRepo + ":v" + version }

// Args are the client's arguments: the image's default command is
// "serve -hosted", and Nomad's args replace it, so they start with it.
// ${NOMAD_SECRETS_DIR} and ${NOMAD_TASK_DIR} are interpolated by Nomad.
func (s JobSettings) Args(tenantID string) []string {
	args := []string{
		"serve", "-hosted", "-live",
		"-addr", fmt.Sprintf("0.0.0.0:%d", ContainerPort), "-hosted-allow-any-bind",
		"-hosted-tenant", tenantID,
		"-hosted-proxy-pubkey", "${NOMAD_TASK_DIR}/" + proxyKeyFile,
		"-credentials-source", "qm",
		"-qm-addr", s.QMAddr,
		"-qm-auth-mount", QMAuthMount,
		"-qm-role", QMRole,
		"-qm-identity-file", "${NOMAD_SECRETS_DIR}/nomad_" + QMIdentity + ".jwt",
		"-hosted-alert-url", s.AlertURL,
		"-hosted-alert-identity-file", "${NOMAD_SECRETS_DIR}/nomad_" + AlertIdentity + ".jwt",
		"-public-url", "https://" + tenantID + "." + s.TenantDomain,
		"-catalog", DataDir + "/catalog.db",
	}
	if s.VAPIDPublicKey != "" {
		args = append(args, "-hosted-vapid-public-key", s.VAPIDPublicKey)
	}
	return args
}

// BuildJob renders the job of one tenant at one image version. The tenant id
// must satisfy tenant.Valid (it becomes part of the job id, the host volume
// name and the public URL). The result is deterministic: MetaSpec holds the
// hash of everything else, so re-registering an unchanged job is skipped.
func BuildJob(s JobSettings, tenantID, version string) (Job, error) {
	if !tenant.Valid(tenantID) {
		return Job{}, ErrInvalidJob
	}
	v, err := NormalizeVersion(version)
	if err != nil {
		return Job{}, err
	}
	if err := s.Validate(); err != nil {
		return Job{}, fmt.Errorf("job settings: %w", err)
	}
	id := JobPrefix + tenantID
	job := Job{
		ID: id, Name: id, Namespace: s.Namespace, Region: s.Region,
		Datacenters: s.Datacenters, Type: "service", Priority: 50,
		Meta: map[string]string{MetaTenant: tenantID, MetaVersion: v},
		TaskGroups: []TaskGroup{{
			Name: GroupName, Count: 1,
			Networks: []Network{{Mode: "bridge", DynamicPorts: []Port{{Label: PortLabel, To: ContainerPort, HostNetwork: s.HostNetwork}}}},
			Volumes:  map[string]VolumeRequest{"data": {Name: "data", Type: "host", Source: VolumeName(tenantID)}},
			// Three restarts in ten minutes, then the allocation fails and
			// is rescheduled with a growing delay, for as long as it takes.
			RestartPolicy:    RestartPolicy{Attempts: 3, Interval: 10 * time.Minute, Delay: 15 * time.Second, Mode: "fail"},
			ReschedulePolicy: ReschedulePolicy{Delay: 30 * time.Second, DelayFunction: "exponential", MaxDelay: time.Hour, Unlimited: true},
			Update: Update{
				MaxParallel: 1, HealthCheck: "checks", MinHealthyTime: minHealthyTime,
				HealthyDeadline: healthyDeadline, ProgressDeadline: progressDeadline, AutoRevert: true,
			},
			Services: []Service{{
				// One shared name: the tenant is in the tag, and a service
				// name is capped at 63 characters where a job id is not.
				Name: "sanholo-bot", PortLabel: PortLabel, Provider: "nomad", AddressMode: "host",
				Tags: []string{"tenant=" + tenantID},
				Checks: []Check{{
					Name: "healthz", Type: "http", Protocol: "http", Method: "GET", Path: "/healthz", PortLabel: PortLabel,
					Interval: checkInterval, Timeout: checkTimeout,
					CheckRestart: CheckRestart{Limit: checkRestartMax, Grace: checkRestartWait},
				}},
			}},
			Tasks: []Task{{
				Name: TaskName, Driver: "docker",
				Config: map[string]any{
					"image":        s.Image(v),
					"args":         s.Args(tenantID),
					"cap_drop":     []string{"ALL"},
					"security_opt": []string{"no-new-privileges"},
				},
				Identities: []Identity{
					{Name: QMIdentity, Audience: []string{QMIdentity}, File: true, TTL: identityTTL},
					{Name: AlertIdentity, Audience: []string{AlertIdentity}, File: true, TTL: identityTTL},
				},
				Templates: []Template{{
					EmbeddedTmpl: strings.TrimSpace(s.ProxyPublicKeys) + "\n",
					DestPath:     "local/" + proxyKeyFile, ChangeMode: "noop", Perms: "0644",
				}},
				VolumeMounts: []VolumeMount{{Volume: "data", Destination: DataDir}},
				Resources:    Resources{CPU: s.CPUMHz, MemoryMB: s.MemoryMB},
				LogConfig:    LogConfig{MaxFiles: 3, MaxFileSizeMB: 10},
				KillTimeout:  killTimeout, KillSignal: "SIGTERM",
			}},
		}},
	}
	spec, err := specHash(job)
	if err != nil {
		return Job{}, err
	}
	job.Meta[MetaSpec] = spec
	return job, nil
}

// specHash hashes the job as rendered without MetaSpec.
func specHash(j Job) (string, error) {
	raw, err := json.Marshal(j)
	if err != nil {
		return "", fmt.Errorf("render job: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
