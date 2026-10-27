package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 32 bytes of 0x01, base64: a well-formed raw Ed25519 public key.
const testProxyKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func nomadEnv(kv ...string) map[string]string {
	m := map[string]string{
		"DATABASE_URL": "postgres://u:p@h/d",
		"QM_ADDR":      "https://qm.sanholo.space", "QM_TOKEN": "dev-token",
		"NOMAD_ADDR": "https://nomad.sanholo.space:4646", "NOMAD_TOKEN": "nomad-token-1234",
		"SANHOLO_IMAGE_VERSION": "0.7.1", "TENANT_PROXY_PUBLIC_KEYS": testProxyKey,
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func TestLoadNomadDefaults(t *testing.T) {
	c, err := Load(env(nomadEnv("APP_BASE_URL", "https://app.sanholo.space")))
	if err != nil {
		t.Fatal(err)
	}
	n := c.Nomad
	if !n.Enabled() || n.Client.Namespace != "sanholo" || n.Version != "0.7.1" || n.Timeout != 15*time.Minute {
		t.Errorf("nomad = %+v", n)
	}
	j := n.Job
	if j.Namespace != "sanholo" || len(j.Datacenters) != 1 || j.Datacenters[0] != "dc1" || j.MemoryMB != 384 || j.CPUMHz != 300 ||
		j.TenantDomain != "sanholo.space" || j.QMAddr != "https://qm.sanholo.space" ||
		j.AlertURL != "https://app.sanholo.space/internal/alerts" || j.ImageRepo != "ghcr.io/tydewhatmay/sanholo" {
		t.Errorf("job settings = %+v", j)
	}
}

func TestLoadNomadOverrides(t *testing.T) {
	c, err := Load(env(nomadEnv(
		"NOMAD_NAMESPACE", "staging", "NOMAD_REGION", "global", "NOMAD_DATACENTERS", "dc1, dc2", "NOMAD_HOST_NETWORK", "private",
		"TENANT_MEMORY_MB", "512", "TENANT_CPU_MHZ", "400", "TENANT_DOMAIN", "staging.sanholo.space",
		"TENANT_ALERT_URL", "https://app.staging.sanholo.space/internal/alerts", "PROVISION_TIMEOUT", "20m",
		"SANHOLO_IMAGE_VERSION", "v0.8.0", "SANHOLO_IMAGE_REPO", "registry.example.org/sanholo/bot",
	)))
	if err != nil {
		t.Fatal(err)
	}
	n := c.Nomad
	if n.Client.Namespace != "staging" || n.Job.Namespace != "staging" || n.Client.Region != "global" || n.Job.Region != "global" ||
		len(n.Job.Datacenters) != 2 || n.Job.Datacenters[1] != "dc2" || n.Job.HostNetwork != "private" ||
		n.Job.MemoryMB != 512 || n.Job.CPUMHz != 400 || n.Version != "0.8.0" || n.Timeout != 20*time.Minute ||
		n.Job.ImageRepo != "registry.example.org/sanholo/bot" {
		t.Errorf("nomad = %+v", n)
	}
}

func TestLoadNomadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"remote http", nomadEnv("NOMAD_ADDR", "http://nomad.example.org:4646"), "NOMAD_ADDR"},
		{"no token", nomadEnv("NOMAD_TOKEN", ""), "NOMAD_TOKEN"},
		{"both tokens", nomadEnv("NOMAD_TOKEN_FILE", "/x"), "NOMAD_TOKEN"},
		{"no version", nomadEnv("SANHOLO_IMAGE_VERSION", ""), "SANHOLO_IMAGE_VERSION"},
		{"latest", nomadEnv("SANHOLO_IMAGE_VERSION", "latest"), "SANHOLO_IMAGE_VERSION"},
		{"no qm", nomadEnv("QM_ADDR", "", "QM_TOKEN", ""), "QM_ADDR"},
		{"no proxy key", nomadEnv("TENANT_PROXY_PUBLIC_KEYS", ""), "TENANT_PROXY_PUBLIC_KEYS"},
		{"both key sources", nomadEnv("TENANT_PROXY_PUBLIC_KEYS_FILE", "/x"), "TENANT_PROXY_PUBLIC_KEYS"},
		{"missing key file", nomadEnv("TENANT_PROXY_PUBLIC_KEYS", "", "TENANT_PROXY_PUBLIC_KEYS_FILE", "/nonexistent/key"), "TENANT_PROXY_PUBLIC_KEYS_FILE"},
		{"garbage key", nomadEnv("TENANT_PROXY_PUBLIC_KEYS", "not a key"), "proxy public keys"},
		{"bad memory", nomadEnv("TENANT_MEMORY_MB", "lots"), "TENANT_MEMORY_MB"},
		{"tiny memory", nomadEnv("TENANT_MEMORY_MB", "8"), "memory"},
		{"bad timeout", nomadEnv("PROVISION_TIMEOUT", "soon"), "PROVISION_TIMEOUT"},
		{"short timeout", nomadEnv("PROVISION_TIMEOUT", "5s"), "PROVISION_TIMEOUT"},
		{"http alert url", nomadEnv("TENANT_ALERT_URL", "http://app.example.org/internal/alerts"), "alert URL"},
		{"token without address", map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "NOMAD_TOKEN": "t"}, "NOMAD_ADDR"},
		{"version without address", map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "SANHOLO_IMAGE_VERSION": "0.7.1"}, "NOMAD_ADDR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
			for _, secret := range []string{"nomad-token-1234", "dev-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error leaks %q", secret)
				}
			}
		})
	}
}

func TestLoadNomadDisabledInDevelopment(t *testing.T) {
	c, err := Load(env(map[string]string{"DATABASE_URL": "postgres://u:p@h/d"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Nomad.Enabled() {
		t.Error("Nomad enabled with no NOMAD_ADDR")
	}
}

func TestLoadNomadRequiredInProduction(t *testing.T) {
	prod := map[string]string{
		"DATABASE_URL": "postgres://u:p@h/d", "APP_ENV": "production", "SMTP_HOST": "send.ahasend.com", "SMTP_USERNAME": "u", "SMTP_PASSWORD": "pw",
		"APP_BASE_URL": "https://app.sanholo.space", "TRUSTED_PROXIES": "10.0.0.0/8", "WEBAUTHN_RP_ID": "sanholo.space",
		"QM_ADDR": "https://qm.sanholo.space", "QM_ROLE_ID": "role-1", "QM_SECRET_ID_FILE": "/run/secrets/qm-secret-id",
	}
	if _, err := Load(env(prod)); err == nil || !strings.Contains(err.Error(), "NOMAD_ADDR is required") {
		t.Fatalf("production without Nomad: %v", err)
	}
	for k, v := range map[string]string{
		"NOMAD_ADDR": "https://nomad.sanholo.space:4646", "NOMAD_TOKEN": "t", "SANHOLO_IMAGE_VERSION": "0.7.1", "TENANT_PROXY_PUBLIC_KEYS": testProxyKey,
	} {
		prod[k] = v
	}
	if _, err := Load(env(prod)); err == nil || !strings.Contains(err.Error(), "PROXY_SIGNING_KEY") {
		t.Fatalf("production without a signing key: %v", err)
	}
	prod["PROXY_SIGNING_KEY"] = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	// Without VAPID keys every alert would be dropped silently.
	if _, err := Load(env(prod)); err == nil || !strings.Contains(err.Error(), "production needs VAPID") {
		t.Fatalf("production without VAPID keys: %v", err)
	}
	pub, priv := vapidPair(t)
	prod["VAPID_PUBLIC_KEY"], prod["VAPID_PRIVATE_KEY"], prod["VAPID_SUBJECT"] = pub, priv, "mailto:ops@tydewhatmay.eu"
	if _, err := Load(env(prod)); err == nil || !strings.Contains(err.Error(), "DELETION_LOG_PATH") {
		t.Fatalf("production without a deletion log: %v", err)
	}
	prod["DELETION_LOG_PATH"] = "/var/lib/sanholo-log/deletions.jsonl"
	if _, err := Load(env(prod)); err == nil || !strings.Contains(err.Error(), "INTERNAL_LISTEN_ADDR") {
		t.Fatalf("production without the platform listener: %v", err)
	}
	maps.Copy(prod, internalEnv(t))
	if _, err := Load(env(prod)); err != nil {
		t.Fatalf("production with Nomad: %v", err)
	}
	delete(prod, "NOMAD_TOKEN")
	if _, err := Load(env(prod)); err == nil {
		t.Fatal("production without a Nomad token")
	}
}

func TestLoadNomadKeysFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.pub")
	if err := os.WriteFile(path, []byte(testProxyKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(nomadEnv("TENANT_PROXY_PUBLIC_KEYS", "", "TENANT_PROXY_PUBLIC_KEYS_FILE", path)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Nomad.Job.ProxyPublicKeys, testProxyKey) {
		t.Error("the key file was not read")
	}
}

func TestLoadRolloutSettings(t *testing.T) {
	c, err := Load(env(nomadEnv("APP_BASE_URL", "https://app.sanholo.space")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Nomad.RolloutBatch != 5 || c.Nomad.RolloutTimeout != 15*time.Minute {
		t.Errorf("defaults = %d, %v; want 5 and the provisioning timeout", c.Nomad.RolloutBatch, c.Nomad.RolloutTimeout)
	}
	c, err = Load(env(nomadEnv("APP_BASE_URL", "https://app.sanholo.space", "PROVISION_TIMEOUT", "20m")))
	if err != nil || c.Nomad.RolloutTimeout != 20*time.Minute {
		t.Errorf("the rollout timeout does not follow PROVISION_TIMEOUT: %v, %v", c.Nomad.RolloutTimeout, err)
	}
	c, err = Load(env(nomadEnv("APP_BASE_URL", "https://app.sanholo.space", "ROLLOUT_BATCH_SIZE", "3", "ROLLOUT_HEALTH_TIMEOUT", "40m")))
	if err != nil || c.Nomad.RolloutBatch != 3 || c.Nomad.RolloutTimeout != 40*time.Minute {
		t.Errorf("overrides = %d, %v, %v", c.Nomad.RolloutBatch, c.Nomad.RolloutTimeout, err)
	}
}

func TestLoadRolloutSettingsRefusals(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"ROLLOUT_BATCH_SIZE", "0", "ROLLOUT_BATCH_SIZE"},
		{"ROLLOUT_BATCH_SIZE", "many", "ROLLOUT_BATCH_SIZE"},
		{"ROLLOUT_BATCH_SIZE", "51", "ROLLOUT_BATCH_SIZE"},
		{"ROLLOUT_HEALTH_TIMEOUT", "10s", "ROLLOUT_HEALTH_TIMEOUT"},
		{"ROLLOUT_HEALTH_TIMEOUT", "7h", "ROLLOUT_HEALTH_TIMEOUT"},
		{"ROLLOUT_HEALTH_TIMEOUT", "soon", "ROLLOUT_HEALTH_TIMEOUT"},
	} {
		_, err := Load(env(nomadEnv("APP_BASE_URL", "https://app.sanholo.space", tc.name, tc.value)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: error %v", tc.name, tc.value, err)
		}
	}
	// Without NOMAD_ADDR the settings have nothing to configure.
	m := map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "ROLLOUT_BATCH_SIZE": "3"}
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ROLLOUT_BATCH_SIZE") {
		t.Errorf("ROLLOUT_BATCH_SIZE without NOMAD_ADDR: %v", err)
	}
}

func TestLoadSoakAndVolumeSettings(t *testing.T) {
	base := "https://app.sanholo.space"
	c, err := Load(env(nomadEnv("APP_BASE_URL", base)))
	if err != nil {
		t.Fatal(err)
	}
	n := c.Nomad
	if n.CanarySoak != time.Hour || n.BatchSoak != 10*time.Minute {
		t.Errorf("soak defaults = %v, %v; want 1h and 10m", n.CanarySoak, n.BatchSoak)
	}
	if n.Volume.CapacityMB != 1024 || n.Volume.Owner != "65532" || n.Volume.NodePool != "" || n.Volume.ConstraintAttr != "" {
		t.Errorf("volume defaults = %+v", n.Volume)
	}
	c, err = Load(env(nomadEnv("APP_BASE_URL", base, "ROLLOUT_CANARY_SOAK", "2h", "ROLLOUT_BATCH_SOAK", "0",
		"TENANT_VOLUME_SIZE_MB", "2048", "TENANT_VOLUME_NODE_POOL", "tenants", "TENANT_VOLUME_CONSTRAINT", "${meta.sanholo}=true")))
	if err != nil {
		t.Fatal(err)
	}
	n = c.Nomad
	if n.CanarySoak != 2*time.Hour || n.BatchSoak != 0 || n.Volume.CapacityMB != 2048 || n.Volume.NodePool != "tenants" ||
		n.Volume.ConstraintAttr != "${meta.sanholo}" || n.Volume.ConstraintValue != "true" {
		t.Errorf("overrides = %v %v %+v", n.CanarySoak, n.BatchSoak, n.Volume)
	}
	for _, tc := range []struct{ name, value string }{
		{"ROLLOUT_CANARY_SOAK", "soon"}, {"ROLLOUT_CANARY_SOAK", "25h"}, {"ROLLOUT_BATCH_SOAK", "-1m"},
		{"TENANT_VOLUME_SIZE_MB", "0"}, {"TENANT_VOLUME_NODE_POOL", "a b"},
		{"TENANT_VOLUME_CONSTRAINT", "nonsense"}, {"TENANT_VOLUME_CONSTRAINT", "meta.x=y"},
	} {
		if _, err := Load(env(nomadEnv("APP_BASE_URL", base, tc.name, tc.value))); err == nil {
			t.Errorf("%s=%s accepted", tc.name, tc.value)
		}
	}
	m := map[string]string{"DATABASE_URL": "postgres://u:p@h/d", "ROLLOUT_CANARY_SOAK": "1h"}
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ROLLOUT_CANARY_SOAK") {
		t.Errorf("ROLLOUT_CANARY_SOAK without NOMAD_ADDR: %v", err)
	}
}
