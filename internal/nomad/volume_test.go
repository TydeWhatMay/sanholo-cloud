package nomad_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lhaig/sanholo-cloud/internal/nomad"
	"github.com/lhaig/sanholo-cloud/internal/nomad/nomadfake"
)

func volumeEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.fake.RemoveVolume("sanholo-tenant-acme-42")
	return e
}

func TestEnsureHostVolumeCreatesOnceWithTheNomadShape(t *testing.T) {
	e := volumeEnv(t)
	ctx := context.Background()
	v := nomadfake.VolumeSettings()
	v.NodePool = "tenants"
	v.ConstraintAttr, v.ConstraintValue = "${meta.sanholo}", "tenants"
	created, err := e.c.EnsureHostVolume(ctx, "acme-42", v)
	if err != nil || !created {
		t.Fatalf("first call: created %v err %v", created, err)
	}
	created, err = e.c.EnsureHostVolume(ctx, "acme-42", v)
	if err != nil || created {
		t.Fatalf("second call: created %v err %v (an existing volume is reused)", created, err)
	}
	if n := e.fake.Count("PUT", "/v1/volume/host/create"); n != 1 {
		t.Errorf("%d create requests, want 1", n)
	}
	spec := e.fake.VolumeSpec("sanholo-tenant-acme-42")
	raw, _ := json.Marshal(spec)
	for _, want := range []string{
		`"PluginID":"mkdir"`, `"Namespace":"sanholo"`, `"NodePool":"tenants"`,
		`"RequestedCapacityMinBytes":1073741824`, `"RequestedCapacityMaxBytes":1073741824`,
		`"AccessMode":"single-node-writer"`, `"AttachmentMode":"file-system"`,
		`"uid":"65532"`, `"gid":"65532"`, `"mode":"0750"`,
		`"LTarget":"${meta.sanholo}"`, `"RTarget":"tenants"`, `"Operand":"="`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("create request lacks %s: %s", want, raw)
		}
	}
}

func TestDeleteHostVolume(t *testing.T) {
	e := volumeEnv(t)
	ctx := context.Background()
	if err := e.c.DeleteHostVolume(ctx, "acme-42"); err != nil {
		t.Fatalf("deleting a volume that is not there: %v", err)
	}
	if n := e.fake.Count("DELETE", "/v1/volume/host/"); n != 0 {
		t.Errorf("%d delete requests for an absent volume", n)
	}
	if _, err := e.c.EnsureHostVolume(ctx, "acme-42", nomadfake.VolumeSettings()); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteHostVolume(ctx, "acme-42"); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Volumes(); len(got) != 0 {
		t.Errorf("volumes left: %v", got)
	}
}

// Nomad refuses to delete a volume a live job holds, so the caller has to
// stop the job first; the client reports the refusal.
func TestDeleteHostVolumeRefusedWhileTheJobRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.c.Register(ctx, e.job(t, "0.7.1"), nomad.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteHostVolume(ctx, "acme-42"); err == nil {
		t.Fatal("the volume of a running job was deleted")
	}
	if err := e.c.Stop(ctx, testJob, false); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteHostVolume(ctx, "acme-42"); err != nil {
		t.Fatalf("after the stop: %v", err)
	}
}

func TestVolumeRefusesAnythingButATenantID(t *testing.T) {
	e := newEnv(t)
	before := len(e.fake.Requests())
	if _, err := e.c.EnsureHostVolume(context.Background(), "../other", nomadfake.VolumeSettings()); err == nil {
		t.Error("created a volume for an invalid tenant id")
	}
	if err := e.c.DeleteHostVolume(context.Background(), "Bad Id"); err == nil {
		t.Error("deleted a volume for an invalid tenant id")
	}
	if len(e.fake.Requests()) != before {
		t.Error("requests were sent for invalid ids")
	}
}

func TestVolumeSettingsValidate(t *testing.T) {
	ok := nomadfake.VolumeSettings()
	for name, mut := range map[string]func(*nomad.VolumeSettings){
		"no size":          func(v *nomad.VolumeSettings) { v.CapacityMB = 0 },
		"bad pool":         func(v *nomad.VolumeSettings) { v.NodePool = "a b" },
		"attr only":        func(v *nomad.VolumeSettings) { v.ConstraintAttr = "${meta.x}" },
		"attr not braced":  func(v *nomad.VolumeSettings) { v.ConstraintAttr, v.ConstraintValue = "meta.x", "y" },
		"owner not number": func(v *nomad.VolumeSettings) { v.Owner = "root" },
	} {
		v := ok
		mut(&v)
		if v.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid settings refused: %v", err)
	}
}

// The job mounts the dynamic volume by name, with the capability it was
// created with.
func TestJobMountsTheDynamicVolume(t *testing.T) {
	g := group(jobOf(t, nomadfake.Settings(), "acme-42"))
	v := g["Volumes"].(map[string]any)["data"].(map[string]any)
	if v["Type"] != "host" || v["Source"] != "sanholo-tenant-acme-42" ||
		v["AccessMode"] != nomad.VolumeAccessMode || v["AttachmentMode"] != nomad.VolumeAttachmentMode {
		t.Errorf("volume request = %v", v)
	}
}
