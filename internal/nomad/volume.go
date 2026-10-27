package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

// Dynamic host volumes (Nomad 1.10+). The request and response shapes below
// were checked against a Nomad 2.0.6 dev agent: PUT /v1/volume/host/create
// answers {"Volume": {"ID", "Name", "State", ...}}; creating a name that
// already exists is a 500 ("no node meets constraints ... had existing
// volume"), so the volume is looked up first; GET /v1/volumes?type=host with
// a filter expression lists by name; DELETE /v1/volume/host/<id>/delete of an
// unknown id is a 500, so deletion looks the volume up first as well.
const (
	// VolumePlugin is the built-in plugin that makes a directory.
	VolumePlugin = "mkdir"
	// VolumeAccessMode and VolumeAttachmentMode are the capability the
	// volume is created with and the job asks for.
	VolumeAccessMode     = "single-node-writer"
	VolumeAttachmentMode = "file-system"
	// VolumeOwner is the uid and gid of the image's nonroot user. The mkdir
	// plugin takes uid and gid parameters (confirmed: it tries the chown), so
	// the directory is writable by the bot. Changing the owner needs the
	// Nomad client to run as root, which a production client does.
	VolumeOwner = "65532"
	volumeMode  = "0750"
)

// VolumeSettings are the deployment-wide values of a tenant volume.
type VolumeSettings struct {
	// CapacityMB is the size requested for each volume. The mkdir plugin
	// does not enforce it; it is recorded and used for placement.
	CapacityMB int
	// NodePool restricts placement to a node pool; empty means any.
	NodePool string
	// ConstraintAttr and ConstraintValue, when set, restrict placement to
	// nodes whose attribute (for example ${meta.sanholo_tenants}) equals the
	// value.
	ConstraintAttr  string
	ConstraintValue string
	// Owner is the uid and gid the directory is given (VolumeOwner in
	// production). Empty leaves the plugin's default, for a Nomad client
	// that does not run as root (tests against a dev agent).
	Owner string
}

var ownerRE = regexp.MustCompile(`^[0-9]{1,10}$`)
var constraintAttrRE = regexp.MustCompile(`^\$\{[a-zA-Z0-9_.-]{1,64}\}$`)
var constraintValueRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate checks the settings.
func (v VolumeSettings) Validate() error {
	var errs []error
	if v.CapacityMB < 1 || v.CapacityMB > 1<<20 {
		errs = append(errs, errors.New("volume size: want 1 MB to 1 TB"))
	}
	if v.NodePool != "" && !hostNetRE.MatchString(v.NodePool) {
		errs = append(errs, errors.New("volume node pool: invalid name"))
	}
	if v.Owner != "" && !ownerRE.MatchString(v.Owner) {
		errs = append(errs, errors.New("volume owner: want a numeric id"))
	}
	if (v.ConstraintAttr == "") != (v.ConstraintValue == "") {
		errs = append(errs, errors.New("volume constraint: set both the attribute and the value"))
	} else if v.ConstraintAttr != "" && (!constraintAttrRE.MatchString(v.ConstraintAttr) || !constraintValueRE.MatchString(v.ConstraintValue)) {
		errs = append(errs, errors.New("volume constraint: invalid attribute or value"))
	}
	return errors.Join(errs...)
}

type wireConstraint struct {
	LTarget string `json:"LTarget"`
	RTarget string `json:"RTarget"`
	Operand string `json:"Operand"`
}

type wireCapability struct {
	AccessMode     string `json:"AccessMode"`
	AttachmentMode string `json:"AttachmentMode"`
}

type hostVolumeCreate struct {
	Name                      string            `json:"Name"`
	Namespace                 string            `json:"Namespace"`
	PluginID                  string            `json:"PluginID"`
	NodePool                  string            `json:"NodePool,omitempty"`
	Constraints               []wireConstraint  `json:"Constraints,omitempty"`
	RequestedCapacityMinBytes int64             `json:"RequestedCapacityMinBytes"`
	RequestedCapacityMaxBytes int64             `json:"RequestedCapacityMaxBytes"`
	RequestedCapabilities     []wireCapability  `json:"RequestedCapabilities"`
	Parameters                map[string]string `json:"Parameters"`
}

type volumeStub struct {
	ID    string `json:"ID"`
	Name  string `json:"Name"`
	State string `json:"State"`
}

// hostVolumeID finds the volume by name; "" when there is none.
func (c *Client) hostVolumeID(ctx context.Context, name string) (string, error) {
	q := url.Values{}
	q.Set("type", "host")
	q.Set("filter", fmt.Sprintf("Name == %q", name))
	var stubs []volumeStub
	if err := c.do(ctx, request{op: "volume", method: http.MethodGet, path: "/v1/volumes", query: q}, &stubs); err != nil {
		return "", err
	}
	for _, s := range stubs {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return "", nil
}

func (c *Client) volumeCheck(tenantID string) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}
	if _, err := JobID(tenantID); err != nil {
		return "", &Error{Op: "volume", Err: ErrInvalidJob}
	}
	return VolumeName(tenantID), nil
}

// EnsureHostVolume creates the tenant's volume unless one with its name
// exists. It reports whether it created one.
func (c *Client) EnsureHostVolume(ctx context.Context, tenantID string, v VolumeSettings) (created bool, err error) {
	name, err := c.volumeCheck(tenantID)
	if err != nil {
		return false, err
	}
	id, err := c.hostVolumeID(ctx, name)
	if err != nil {
		return false, err
	}
	if id != "" {
		return false, nil
	}
	size := int64(v.CapacityMB) << 20
	vol := hostVolumeCreate{
		Name: name, Namespace: c.namespace, PluginID: VolumePlugin, NodePool: v.NodePool,
		RequestedCapacityMinBytes: size, RequestedCapacityMaxBytes: size,
		RequestedCapabilities: []wireCapability{{AccessMode: VolumeAccessMode, AttachmentMode: VolumeAttachmentMode}},
		Parameters:            map[string]string{"mode": volumeMode},
	}
	if v.Owner != "" {
		vol.Parameters["uid"], vol.Parameters["gid"] = v.Owner, v.Owner
	}
	if v.ConstraintAttr != "" {
		vol.Constraints = []wireConstraint{{LTarget: v.ConstraintAttr, RTarget: v.ConstraintValue, Operand: "="}}
	}
	body := map[string]any{"Volume": vol}
	if err := c.do(ctx, request{op: "volume create", method: http.MethodPut, path: "/v1/volume/host/create", body: body}, nil); err != nil {
		return false, err
	}
	c.log.Info("nomad: host volume created", "volume", name)
	return true, nil
}

// DeleteHostVolume deletes the tenant's volume; one that is gone is not an
// error. Nomad refuses while an allocation still claims the volume, so the
// job is stopped (and purged) first.
func (c *Client) DeleteHostVolume(ctx context.Context, tenantID string) error {
	name, err := c.volumeCheck(tenantID)
	if err != nil {
		return err
	}
	id, err := c.hostVolumeID(ctx, name)
	if err != nil || id == "" {
		return err
	}
	err = c.do(ctx, request{op: "volume delete", method: http.MethodDelete, path: "/v1/volume/host/" + url.PathEscape(id) + "/delete"}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err == nil {
		c.log.Info("nomad: host volume deleted", "volume", name)
	}
	return err
}

// HostVolumeExists reports whether the tenant's volume exists.
func (c *Client) HostVolumeExists(ctx context.Context, tenantID string) (bool, error) {
	name, err := c.volumeCheck(tenantID)
	if err != nil {
		return false, err
	}
	id, err := c.hostVolumeID(ctx, name)
	return id != "", err
}
