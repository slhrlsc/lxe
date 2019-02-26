package cri

import (
	"os"
	"os/user"
	"path"
	"strings"
	"time"

	"github.com/lxc/lxe/lxf"
	rtApi "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

const (
	// fieldLXEBridge is the key name to specify the bridge to be used as parent
	// TODO: to be removed once specifyable with CNI
	fieldLXEBridge = "x-lxe-bridge"
	// fieldLXEAdditionalLXDConfig is the name of the field which contains various additional lxd config options
	fieldLXEAdditionalLXDConfig = "x-lxe-additional-lxd-config"
)

// AdditionalLXDConfig contains additional config options not present in PodSpec
// Key names and values must match the key names specified by LXD
type AdditionalLXDConfig map[string]string

func toCriStatusResponse(c *lxf.Container) *rtApi.ContainerStatusResponse {
	status := rtApi.ContainerStatus{
		Metadata: &rtApi.ContainerMetadata{
			Name:    c.Metadata.Name,
			Attempt: uint32(c.Metadata.Attempt),
		},
		State:       stateContainerAsCri(c.State.Name),
		CreatedAt:   c.CreatedAt.UnixNano(),
		StartedAt:   c.StartedAt.UnixNano(),
		FinishedAt:  c.FinishedAt.UnixNano(),
		Id:          c.ID,
		Labels:      c.Labels,
		Annotations: c.Annotations,
		Image:       &rtApi.ImageSpec{Image: c.Image},
		ImageRef:    c.Image,
	}

	return &rtApi.ContainerStatusResponse{
		Status: &status,
		Info:   map[string]string{},
	}
}

func toCriStats(c *lxf.Container) *rtApi.ContainerStats {
	now := time.Now().UnixNano()

	cpu := rtApi.CpuUsage{
		Timestamp:            now,
		UsageCoreNanoSeconds: &rtApi.UInt64Value{Value: c.State.Stats.CPUUsage},
	}
	memory := rtApi.MemoryUsage{
		Timestamp:       now,
		WorkingSetBytes: &rtApi.UInt64Value{Value: c.State.Stats.MemoryUsage},
	}
	disk := rtApi.FilesystemUsage{
		Timestamp: now,
		UsedBytes: &rtApi.UInt64Value{Value: c.State.Stats.FilesystemUsage},
	}
	attribs := rtApi.ContainerAttributes{
		Id: c.ID,
		Metadata: &rtApi.ContainerMetadata{
			Name:    c.Metadata.Name,
			Attempt: uint32(c.Metadata.Attempt),
		},
		Labels:      c.Labels,
		Annotations: c.Annotations,
	}

	response := rtApi.ContainerStats{
		Cpu:           &cpu,
		Memory:        &memory,
		WritableLayer: &disk,
		Attributes:    &attribs,
	}
	return &response
}

func toCriContainer(c *lxf.Container) *rtApi.Container {

	return &rtApi.Container{
		Id:           c.ID,
		PodSandboxId: c.Profiles[0],
		Image:        &rtApi.ImageSpec{Image: c.Image},
		ImageRef:     c.Image,
		CreatedAt:    c.CreatedAt.UnixNano(),
		State:        stateContainerAsCri(c.State.Name),
		Metadata: &rtApi.ContainerMetadata{
			Name:    c.Metadata.Name,
			Attempt: uint32(c.Metadata.Attempt),
		},
		Labels:      c.Labels,
		Annotations: c.Annotations,
	}
	// TODO: more fields?
}

func stateContainerAsCri(s lxf.ContainerStateName) rtApi.ContainerState {
	return rtApi.ContainerState(
		rtApi.ContainerState_value["CONTAINER_"+strings.ToUpper(s.String())])
}

func stateSandboxAsCri(s lxf.SandboxState) rtApi.PodSandboxState {
	return rtApi.PodSandboxState(
		rtApi.PodSandboxState_value["SANDBOX_"+strings.ToUpper(s.String())])
}

func nameSpaceOptionToString(no rtApi.NamespaceMode) string {
	return strings.ToLower(no.String())
}

func stringToNamespaceOption(s string) rtApi.NamespaceMode {
	return rtApi.NamespaceMode(rtApi.NamespaceMode_value[strings.ToUpper(s)])
}

// CompareFilterMap allows comparing two string maps
func CompareFilterMap(base map[string]string, filter map[string]string) bool {
	if filter == nil { // filter can be nil
		return true
	}
	for key := range filter {
		if base[key] != filter[key] {
			return false
		}
	}
	return true
}

// getLXDConfigPath tries to find the remote configuration file path
func getLXDConfigPath(cfg *LXEConfig) (string, error) {
	configPath := cfg.LXDRemoteConfig
	if cfg.LXDRemoteConfig == "" {
		// Copied from github.com/lxc/lxd/lxc/main.go:56, since there it is unexported
		var configDir string
		if os.Getenv("LXD_CONF") != "" {
			configDir = os.Getenv("LXD_CONF")
		} else if os.Getenv("HOME") != "" {
			configDir = path.Join(os.Getenv("HOME"), ".config", "lxc")
		} else {
			user, err := user.Current()
			if err != nil {
				return "", err
			}
			configDir = path.Join(user.HomeDir, ".config", "lxc")
		}
		configPath = os.ExpandEnv(path.Join(configDir, "config.yml"))
	}
	return configPath, nil
}

func (s RuntimeServer) stopContainers(sb *lxf.Sandbox) error {
	cl, err := sb.Containers()
	if err != nil {
		return err
	}
	for _, c := range cl {
		err := s.stopContainer(c, 30)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s RuntimeServer) stopContainer(c *lxf.Container, timeout int) error {
	err := c.Stop(timeout)
	if err != nil {
		if lxf.IsContainerNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func (s RuntimeServer) deleteContainers(sb *lxf.Sandbox) error {
	cl, err := sb.Containers()
	if err != nil {
		return err
	}
	for _, c := range cl {
		err = s.deleteContainer(c)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s RuntimeServer) deleteContainer(c *lxf.Container) error {
	return c.Delete()
}
