package cri

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/automaticserver/lxe/lxf"
	"github.com/automaticserver/lxe/lxf/device"
	"github.com/automaticserver/lxe/network"
	"github.com/automaticserver/lxe/shared"
	"github.com/docker/docker/pkg/pools"
	"github.com/lxc/lxd/lxc/config"
	"github.com/lxc/lxd/shared/logger"
	opencontainers "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	utilNet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/tools/remotecommand"
	rtApi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/pkg/kubelet/util/ioutils"
	utilExec "k8s.io/utils/exec"
)

const (
	criVersion = "0.1.0"
)

var (
	ErrNotImplemented       = errors.New("not implemented")
	ErrUnknownNetworkPlugin = errors.New("unknown network plugin")
)

// streamService implements streaming.Runtime.
type streamService struct {
	streaming.Runtime
	runtimeServer       *RuntimeServer // needed by Exec() endpoint
	streamServer        streaming.Server
	streamServerCloseCh chan struct{}
}

// RuntimeServer is the PoC implementation of the CRI RuntimeServer
type RuntimeServer struct {
	rtApi.RuntimeServiceServer
	lxf       lxf.Client
	stream    streamService
	lxdConfig *config.Config
	criConfig *Config
	network   network.Plugin
}

// NewRuntimeServer returns a new RuntimeServer backed by LXD
func NewRuntimeServer(criConfig *Config, lxf lxf.Client, network network.Plugin) (*RuntimeServer, error) {
	var err error

	runtime := RuntimeServer{
		criConfig: criConfig,
		network:   network,
	}

	configPath, err := getLXDConfigPath(criConfig)
	if err != nil {
		return nil, err
	}

	runtime.lxdConfig, err = config.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	runtime.lxf = lxf
	streamServerAddr := criConfig.LXEStreamingServerEndpoint + ":" + strconv.Itoa(criConfig.LXEStreamingPort)

	outboundIP, err := utilNet.ChooseHostInterface()
	if err != nil {
		logger.Errorf("could not find suitable host interface: %v", err)
		return nil, err
	}

	// Prepare streaming server
	streamServerConfig := streaming.DefaultConfig
	streamServerConfig.Addr = streamServerAddr
	streamServerConfig.BaseURL = &url.URL{
		Scheme: "http",
		Host:   outboundIP.String() + ":" + strconv.Itoa(criConfig.LXEStreamingPort),
	}
	runtime.stream.runtimeServer = &runtime

	runtime.stream.streamServer, err = streaming.NewServer(streamServerConfig, runtime.stream)
	if err != nil {
		logger.Errorf("unable to create streaming server")
		return nil, err
	}

	runtime.stream.streamServerCloseCh = make(chan struct{})

	go func() {
		defer close(runtime.stream.streamServerCloseCh)
		logger.Infof("Starting streaming server on %v", streamServerConfig.Addr)

		err := runtime.stream.streamServer.Start(true)
		if err != nil {
			panic(fmt.Errorf("error serving execs or portforwards: %w", err))
		}
	}()

	return &runtime, nil
}

// Version returns the runtime name, runtime version, and runtime API version.
func (s RuntimeServer) Version(ctx context.Context, req *rtApi.VersionRequest) (*rtApi.VersionResponse, error) {
	logger.Debugf("Version triggered: %v", req)

	// according to containerd CRI implementation RuntimeName=ShimName, RuntimeVersion=ShimVersion,
	// RuntimeApiVersion=someAPIVersion. The actual runtime name and version is not present
	info, err := s.lxf.GetRuntimeInfo()
	if err != nil {
		logger.Errorf("unable to get server environment: %v", err)
		return nil, err
	}

	response := &rtApi.VersionResponse{
		Version:           criVersion,
		RuntimeName:       Domain,
		RuntimeVersion:    Version,
		RuntimeApiVersion: info.Version,
	}

	logger.Debugf("Version responded: %v", response)

	return response, nil
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes must ensure the sandbox is in the ready state on
// success
func (s RuntimeServer) RunPodSandbox(ctx context.Context, req *rtApi.RunPodSandboxRequest) (*rtApi.RunPodSandboxResponse, error) { // nolint: gocognit
	logger.Infof("RunPodSandbox called: SandboxName %v in Namespace %v with SandboxUID %v", req.GetConfig().GetMetadata().GetName(),
		req.GetConfig().GetMetadata().GetNamespace(), req.GetConfig().GetMetadata().GetUid())
	logger.Debugf("RunPodSandbox triggered: %v", req)

	var err error

	sb := s.lxf.NewSandbox()

	sb.Hostname = req.GetConfig().GetHostname()
	sb.LogDirectory = req.GetConfig().GetLogDirectory()
	meta := req.GetConfig().GetMetadata()
	sb.Metadata = lxf.SandboxMetadata{
		Attempt:   meta.GetAttempt(),
		Name:      meta.GetName(),
		Namespace: meta.GetNamespace(),
		UID:       meta.GetUid(),
	}
	sb.Labels = req.GetConfig().GetLabels()
	sb.Annotations = req.GetConfig().GetAnnotations()

	if req.GetConfig().GetDnsConfig() != nil {
		sb.NetworkConfig.Nameservers = req.GetConfig().GetDnsConfig().GetServers()
		sb.NetworkConfig.Searches = req.GetConfig().GetDnsConfig().GetSearches()
	}

	// Find out which network mode should be used
	if strings.ToLower(req.GetConfig().GetLinux().GetSecurityContext().GetNamespaceOptions().GetNetwork().String()) == string(lxf.NetworkHost) {
		// host network explicitly requested
		sb.NetworkConfig.Mode = lxf.NetworkHost
		lxf.AppendIfSet(&sb.Config, "raw.lxc", "lxc.include = "+s.criConfig.LXEHostnetworkFile)
	} else {
		// manage network according to selected network plugin
		// TODO: we could omit these since we use network plugin, but we still need to remember if it is HostNetwork
		switch s.criConfig.LXENetworkPlugin {
		case NetworkPluginDefault:
			sb.NetworkConfig.Mode = lxf.NetworkBridged
		case NetworkPluginCNI:
			sb.NetworkConfig.Mode = lxf.NetworkCNI
		default:
			// unknown plugin name provided
			err := fmt.Errorf("%w: %v", ErrUnknownNetworkPlugin, s.criConfig.LXENetworkPlugin)
			logger.Error(err.Error())
			return nil, err
		}
	}

	// If HostPort is defined, set forwardings from that port to the container. In lxd, we can use proxy devices for that.
	// This can be applied to all NetworkModes except HostNetwork.
	if sb.NetworkConfig.Mode != lxf.NetworkHost {
		for _, portMap := range req.Config.PortMappings {
			// both HostPort and ContainerPort must be defined, otherwise invalid
			if portMap.GetHostPort() == 0 || portMap.GetContainerPort() == 0 {
				continue
			}

			hostPort := int(portMap.GetHostPort())
			containerPort := int(portMap.GetContainerPort())

			var protocol device.Protocol

			switch portMap.GetProtocol() { // nolint: exhaustive
			case rtApi.Protocol_UDP:
				protocol = device.ProtocolUDP
			case rtApi.Protocol_TCP:
				fallthrough
			default:
				protocol = device.ProtocolTCP
			}

			hostIP := portMap.GetHostIp()
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}

			containerIP := "127.0.0.1"

			sb.Devices.Upsert(&device.Proxy{
				Listen: &device.ProxyEndpoint{
					Protocol: protocol,
					Address:  hostIP,
					Port:     hostPort,
				},
				Destination: &device.ProxyEndpoint{
					Protocol: protocol,
					Address:  containerIP,
					Port:     containerPort,
				},
			})
		}
	}

	// TODO: Refactor...
	if req.Config.Linux != nil { // nolint: nestif
		lxf.SetIfSet(&sb.Config, "user.linux.cgroup_parent", req.Config.Linux.CgroupParent)

		for key, value := range req.Config.Linux.Sysctls {
			sb.Config["user.linux.sysctls."+key] = value
		}

		if req.Config.Linux.SecurityContext != nil {
			privileged := req.Config.Linux.SecurityContext.Privileged
			sb.Config["user.linux.security_context.privileged"] = strconv.FormatBool(privileged)
			sb.Config["security.privileged"] = strconv.FormatBool(privileged)

			if req.Config.Linux.SecurityContext.NamespaceOptions != nil {
				nsi := "user.linux.security_context.namespace_options"
				nso := req.Config.Linux.SecurityContext.NamespaceOptions

				sb.Config[nsi+".ipc"] = nameSpaceOptionToString(nso.Ipc)
				sb.Config[nsi+".network"] = nameSpaceOptionToString(nso.Network)
				sb.Config[nsi+".pid"] = nameSpaceOptionToString(nso.Pid)
			}

			if req.Config.Linux.SecurityContext.ReadonlyRootfs {
				sb.Devices.Upsert(&device.Disk{
					Path:     "/",
					Readonly: true,
					// TODO magic constant, and also, is it always default?
					Pool: "default",
				})
			}

			if req.Config.Linux.SecurityContext.RunAsUser != nil {
				sb.Config["user.linux.security_context.run_as_user"] =
					strconv.FormatInt(req.Config.Linux.SecurityContext.RunAsUser.Value, 10)
			}

			lxf.SetIfSet(&sb.Config, "user.linux.security_context.seccomp_profile_path",
				req.Config.Linux.SecurityContext.SeccompProfilePath)

			if req.Config.Linux.SecurityContext.SelinuxOptions != nil {
				sci := "user.linux.security_context.namespace_options"
				sco := req.Config.Linux.SecurityContext.SelinuxOptions
				lxf.SetIfSet(&sb.Config, sci+".role", sco.Role)
				lxf.SetIfSet(&sb.Config, sci+".level", sco.Level)
				lxf.SetIfSet(&sb.Config, sci+".user", sco.User)
				lxf.SetIfSet(&sb.Config, sci+".type", sco.Type)
			}
		}
	}

	err = sb.Apply()
	if err != nil {
		logger.Errorf("RunPodSandbox: SandboxName %v failed to create sandbox: %v", req.GetConfig().GetMetadata().GetName(), err)
		return nil, err
	}

	// create network
	if sb.NetworkConfig.Mode != lxf.NetworkHost { // nolint: nestif
		podNet, err := s.network.PodNetwork(sb.ID, sb.Annotations)
		if err != nil {
			err := errors.Wrap(err, fmt.Sprintf("can't enter sandbox %v network context", sb.ID))
			logger.Error(err.Error())

			return nil, err
		}

		res, err := podNet.WhenCreated(ctx, &network.Properties{})
		if err != nil {
			err := errors.Wrap(err, fmt.Sprintf("can't create sandbox %v network context", sb.ID))
			logger.Error(err.Error())

			return nil, err
		}

		err = s.handleNetworkResult(sb, res)
		if err != nil {
			err := errors.Wrap(err, fmt.Sprintf("can't save create sandbox %v network result", sb.ID))
			logger.Error(err.Error())

			return nil, err
		}

		// Since a PodSandbox is created "started", also fire started network
		res, err = podNet.WhenStarted(ctx, &network.PropertiesRunning{
			Properties: network.Properties{
				Data: sb.NetworkConfig.ModeData,
			},
			Pid: 0, // if we had real 1:n pod:container we would add here the pid of the pod process
		})
		if err != nil {
			err := errors.Wrap(err, fmt.Sprintf("can't start sandbox %v network context", sb.ID))
			logger.Error(err.Error())

			return nil, err
		}

		err = s.handleNetworkResult(sb, res)
		if err != nil {
			err := errors.Wrap(err, fmt.Sprintf("can't save start sandbox %v network result", sb.ID))
			logger.Error(err.Error())

			return nil, err
		}
	}

	logger.Infof("RunPodSandbox successful: Created SandboxID %v for SandboxUID %v", sb.ID, req.GetConfig().GetMetadata().GetUid())

	response := &rtApi.RunPodSandboxResponse{
		PodSandboxId: sb.ID,
	}

	logger.Debugf("RunPodSandbox responded: %v", response)

	return response, nil
}

// StopPodSandbox stops any running process that is part of the sandbox and reclaims network resources (e.g. IP
// addresses) allocated to the sandbox. If there are any running containers in the sandbox, they must be forcibly
// terminated. This call is idempotent, and must not return an error if all relevant resources have already been
// reclaimed. kubelet will call StopPodSandbox at least once before calling RemovePodSandbox. It will also attempt to
// reclaim resources eagerly, as soon as a sandbox is not needed. Hence, multiple StopPodSandbox calls are expected.
func (s RuntimeServer) StopPodSandbox(ctx context.Context, req *rtApi.StopPodSandboxRequest) (*rtApi.StopPodSandboxResponse, error) {
	logger.Infof("StopPodSandbox called: SandboxID %v", req.GetPodSandboxId())
	logger.Debugf("StopPodSandbox triggered: %v", req)

	sb, err := s.lxf.GetSandbox(req.GetPodSandboxId())
	if err != nil {
		// If the sandbox can't be found, return no error with empty result
		if shared.IsErrNotFound(err) {
			return &rtApi.StopPodSandboxResponse{}, nil
		}

		logger.Errorf("StopPodSandbox: SandboxID %v Trying to get sandbox: %v", req.GetPodSandboxId(), err)

		return nil, err
	}

	err = s.stopContainers(sb)
	if err != nil {
		logger.Errorf("StopPodSandbox: SandboxID %v Trying to stop containers: %v", req.GetPodSandboxId(), err)
		return nil, err
	}

	err = sb.Stop()
	if err != nil {
		logger.Errorf("StopPodSandbox: SandboxID %v Trying to stop: %v", req.GetPodSandboxId(), err)
		return nil, err
	}

	// Stop networking
	if sb.NetworkConfig.Mode != lxf.NetworkHost {
		netw, err := s.network.PodNetwork(sb.ID, sb.Annotations)
		if err == nil { // force cleanup, we don't care about error, but only enter if there's no error
			_ = netw.WhenStopped(ctx, &network.Properties{Data: sb.NetworkConfig.ModeData})
		}
	}

	logger.Infof("StopPodSandbox successful: SandboxID %v", req.GetPodSandboxId())

	response := &rtApi.StopPodSandboxResponse{}

	logger.Debugf("StopPodSandbox responded: %v", response)

	return response, nil
}

// RemovePodSandbox removes the sandbox. This is pretty much the same as StopPodSandbox but also removes the sandbox and
// the containers
func (s RuntimeServer) RemovePodSandbox(ctx context.Context, req *rtApi.RemovePodSandboxRequest) (*rtApi.RemovePodSandboxResponse, error) {
	logger.Infof("RemovePodSandbox called: SandboxID %v", req.GetPodSandboxId())
	logger.Debugf("RemovePodSandbox triggered: %v", req)

	sb, err := s.lxf.GetSandbox(req.GetPodSandboxId())
	if err != nil {
		// If the sandbox can't be found, return no error with empty result
		if shared.IsErrNotFound(err) {
			return &rtApi.RemovePodSandboxResponse{}, nil
		}

		logger.Errorf("RemovePodSandbox: SandboxID %v Trying to get sandbox: %v", req.GetPodSandboxId(), err)

		return nil, err
	}

	err = s.stopContainers(sb)
	if err != nil {
		logger.Errorf("RemovePodSandbox: SandboxID %v Trying to stop containers: %v", req.GetPodSandboxId(), err)
		return nil, err
	}

	err = s.deleteContainers(ctx, sb)
	if err != nil {
		logger.Errorf("RemovePodSandbox: SandboxID %v Trying to delete containers: %v", req.GetPodSandboxId(), err)
		return nil, err
	}

	err = sb.Delete()
	if err != nil {
		logger.Errorf("RemovePodSandbox: SandboxID %v Trying to delete: %v", req.GetPodSandboxId(), err)
		return nil, err
	}

	// Delete networking
	if sb.NetworkConfig.Mode != lxf.NetworkHost {
		netw, err := s.network.PodNetwork(sb.ID, sb.Annotations)
		if err == nil { // we don't care about error, but only enter if there's no error
			_ = netw.WhenDeleted(ctx, &network.Properties{Data: sb.NetworkConfig.ModeData})
		}
	}

	logger.Infof("RemovePodSandbox successful: SandboxID %v", req.GetPodSandboxId())

	response := &rtApi.RemovePodSandboxResponse{}

	logger.Debugf("RemovePodSandbox responded: %v", response)

	return response, nil
}

// PodSandboxStatus returns the status of the PodSandbox. If the PodSandbox is not present, returns an error.
func (s RuntimeServer) PodSandboxStatus(ctx context.Context, req *rtApi.PodSandboxStatusRequest) (*rtApi.PodSandboxStatusResponse, error) {
	//logger.Infof("PodSandboxStatus called: SandboxID %v", req.GetPodSandboxId())
	logger.Debugf("PodSandboxStatus triggered: %v", req)

	sb, err := s.lxf.GetSandbox(req.GetPodSandboxId())
	if err != nil {
		logger.Errorf("PodSandboxStatus: SandboxID %v Trying to get sandbox: %v", req.GetPodSandboxId(), err)
		return nil, err
	}

	response := &rtApi.PodSandboxStatusResponse{
		Status: &rtApi.PodSandboxStatus{
			Id: sb.ID,
			Metadata: &rtApi.PodSandboxMetadata{
				Attempt:   sb.Metadata.Attempt,
				Name:      sb.Metadata.Name,
				Namespace: sb.Metadata.Namespace,
				Uid:       sb.Metadata.UID,
			},
			Linux:       &rtApi.LinuxPodSandboxStatus{},
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
			CreatedAt:   sb.CreatedAt.UnixNano(),
			State:       stateSandboxAsCri(sb.State),
			Network: &rtApi.PodSandboxNetworkStatus{
				Ip: "",
			},
		},
	}

	for k, v := range sb.Config {
		if strings.HasPrefix(k, "user.linux.security_context.namespace_options.") {
			key := strings.TrimPrefix(k, "user.linux.security_context.namespace_options.")

			if response.Status.Linux.Namespaces == nil {
				response.Status.Linux.Namespaces = &rtApi.Namespace{Options: &rtApi.NamespaceOption{}}
			}

			switch key {
			case "ipc":
				response.Status.Linux.Namespaces.Options.Ipc = stringToNamespaceOption(v)
			case "pid":
				response.Status.Linux.Namespaces.Options.Pid = stringToNamespaceOption(v)
			case "network":
				response.Status.Linux.Namespaces.Options.Network = stringToNamespaceOption(v)
			}
		}
	}

	ip := s.getInetAddress(ctx, sb)
	if ip != "" {
		response.Status.Network.Ip = ip
	}

	logger.Debugf("PodSandboxStatus responded: %v", response)

	return response, nil
}

// getInetAddress returns the ip address of the sandbox. empty string if nothing was found
func (s RuntimeServer) getInetAddress(ctx context.Context, sb *lxf.Sandbox) string {
	switch sb.NetworkConfig.Mode {
	case lxf.NetworkHost:
		ip, err := utilNet.ChooseHostInterface()
		if err != nil {
			logger.Errorf("Couldn't choose host interface: %v", err)
			return ""
		}

		return ip.String()
	case lxf.NetworkNone:
		return ""
	case lxf.NetworkBridged:
		// Look into the container, as coded below
	case lxf.NetworkCNI:
		podNet, err := s.network.PodNetwork(sb.ID, sb.Annotations)
		if err != nil {
			logger.Errorf("Couldn't get cni pod network: %v", err)
			return ""
		}

		status, err := podNet.Status(ctx, &network.PropertiesRunning{Properties: network.Properties{Data: sb.NetworkConfig.ModeData}, Pid: 0})
		if err != nil {
			logger.Errorf("Couldn't get status of cni pod network: %v", err)
			return ""
		}

		if len(status.IPs) > 0 {
			return status.IPs[0].String()
		}
	}

	// If not yet returned, look into the containers interface list and select the address from the default interface
	cl, err := sb.Containers()
	if err != nil {
		logger.Errorf("Couldn't list containers while trying to get inet address: %v", err)
		return ""
	}

	for _, c := range cl {
		// ignore any non-running containers
		if c.StateName != lxf.ContainerStateRunning {
			continue
		}

		// get the ipv4 address of eth0
		ip := c.GetInetAddress([]string{network.DefaultInterface})
		if ip != "" {
			return ip
		}
	}

	return ""
}

// ListPodSandbox returns a list of PodSandboxes.
func (s RuntimeServer) ListPodSandbox(ctx context.Context, req *rtApi.ListPodSandboxRequest) (*rtApi.ListPodSandboxResponse, error) {
	logger.Debugf("ListPodSandbox triggered: %v", req)

	sandboxes, err := s.lxf.ListSandboxes()
	if err != nil {
		logger.Errorf("ListPodSandbox: Trying to list sandbox: %v", err)
		return nil, err
	}

	response := &rtApi.ListPodSandboxResponse{}

	for _, sb := range sandboxes {
		if req.GetFilter() != nil {
			filter := req.GetFilter()
			if filter.GetId() != "" && filter.GetId() != sb.ID {
				continue
			}

			if filter.GetState() != nil && filter.GetState().GetState() != stateSandboxAsCri(sb.State) {
				continue
			}

			if !CompareFilterMap(sb.Labels, filter.GetLabelSelector()) {
				continue
			}
		}

		// TODO: toSandboxCRI()
		pod := rtApi.PodSandbox{
			Id:        sb.ID,
			CreatedAt: sb.CreatedAt.UnixNano(),
			Metadata: &rtApi.PodSandboxMetadata{
				Attempt:   sb.Metadata.Attempt,
				Name:      sb.Metadata.Name,
				Namespace: sb.Metadata.Namespace,
				Uid:       sb.Metadata.UID,
			},
			State:       stateSandboxAsCri(sb.State),
			Labels:      sb.Labels,
			Annotations: sb.Annotations,
		}
		response.Items = append(response.Items, &pod)
	}

	logger.Debugf("ListPodSandbox responded: %v", response)

	return response, nil
}

// CreateContainer creates a new container in specified PodSandbox
func (s RuntimeServer) CreateContainer(ctx context.Context, req *rtApi.CreateContainerRequest) (*rtApi.CreateContainerResponse, error) {
	logger.Infof("CreateContainer called: ContainerName %v for SandboxID %v", req.GetConfig().GetMetadata().GetName(), req.GetPodSandboxId())
	logger.Debugf("CreateContainer triggered: %v", req)

	var err error

	c := s.lxf.NewContainer(req.GetPodSandboxId(), s.criConfig.LXDProfiles...)

	c.Labels = req.GetConfig().GetLabels()
	c.Annotations = req.GetConfig().GetAnnotations()
	meta := req.GetConfig().GetMetadata()
	c.Metadata = lxf.ContainerMetadata{
		Attempt: meta.GetAttempt(),
		Name:    meta.GetName(),
	}
	c.LogPath = req.GetConfig().GetLogPath()
	c.Image = req.GetConfig().GetImage().GetImage()

	for _, mnt := range req.GetConfig().GetMounts() {
		hostPath := mnt.GetHostPath()
		containerPath := mnt.GetContainerPath()
		// cannot use /var/run as most distros symlink that to /run and lxd doesn't like mounts there because of that
		if strings.HasPrefix(containerPath, "/var/run") {
			containerPath = path.Join("/run", strings.TrimPrefix(containerPath, "/var/run"))
		}
		// cannot use /run as most distros mount a tmpfs on top of that so mounts from lxd are not visible in the container
		if strings.HasPrefix(containerPath, "/run") {
			containerPath = path.Join("/mnt", strings.TrimPrefix(containerPath, "/run"))
		}

		c.Devices.Upsert(&device.Disk{
			Path:     containerPath,
			Source:   hostPath,
			Readonly: mnt.GetReadonly(),
			Optional: false,
		})
	}

	for _, dev := range req.GetConfig().GetDevices() {
		c.Devices.Upsert(&device.Block{
			Source: dev.GetHostPath(),
			Path:   dev.GetContainerPath(),
		})
	}

	c.Privileged = req.GetConfig().GetLinux().GetSecurityContext().GetPrivileged()

	// get metadata & cloud-init if defined
	for _, env := range req.GetConfig().GetEnvs() {
		switch {
		case env.GetKey() == "user-data":
			c.CloudInitUserData = env.GetValue()
		case env.GetKey() == "meta-data":
			c.CloudInitMetaData = env.GetValue()
		case env.GetKey() == "network-config":
			c.CloudInitNetworkConfig = env.GetValue()
		default:
			c.Environment[env.GetKey()] = env.GetValue()
		}
	}

	// append other envs below metadata
	if c.CloudInitMetaData != "" && len(c.Environment) > 0 {
		c.CloudInitMetaData += "\n"
	}

	// process limits
	resrc := req.GetConfig().GetLinux().GetResources()
	if resrc != nil {
		c.Resources = &opencontainers.LinuxResources{}
		c.Resources.CPU = &opencontainers.LinuxCPU{}
		c.Resources.Memory = &opencontainers.LinuxMemory{}
		shares := uint64(resrc.CpuShares)
		c.Resources.CPU.Shares = &shares
		c.Resources.CPU.Quota = &resrc.CpuQuota
		period := uint64(resrc.CpuPeriod)
		c.Resources.CPU.Period = &period
		c.Resources.Memory.Limit = &resrc.MemoryLimitInBytes
	}

	err = c.Apply()
	if err != nil {
		logger.Errorf("CreateContainer: ContainerName %v trying to create container: %v", req.GetConfig().GetMetadata().GetName(), err)
		return nil, err
	}

	sb, err := c.Sandbox()
	if err != nil {
		return nil, err
	}

	// create network
	if sb.NetworkConfig.Mode != lxf.NetworkHost {
		podNet, err := s.network.PodNetwork(sb.ID, sb.Annotations)
		if err != nil {
			return nil, err
		}

		contNet, err := podNet.ContainerNetwork(c.ID, c.Annotations)
		if err != nil {
			return nil, err
		}

		res, err := contNet.WhenCreated(ctx, &network.Properties{})
		if err != nil {
			return nil, err
		}

		err = s.handleNetworkResult(sb, res)
		if err != nil {
			return nil, err
		}
	}

	logger.Infof("CreateContainer successful: Created ContainerID %v for SandboxID %v", c.ID, req.GetPodSandboxId())

	response := &rtApi.CreateContainerResponse{
		ContainerId: c.ID,
	}

	logger.Debugf("CreateContainer responded: %v", response)

	return response, nil
}

// StartContainer starts the container.
// nolint: dupl
func (s RuntimeServer) StartContainer(ctx context.Context, req *rtApi.StartContainerRequest) (*rtApi.StartContainerResponse, error) {
	logger.Infof("StartContainer called: ContainerID %v", req.GetContainerId())
	logger.Debugf("StartContainer triggered: %v", req)

	c, err := s.lxf.GetContainer(req.GetContainerId())
	if err != nil {
		logger.Errorf("StartContainer: ContainerID %v trying to get container: %v", req.GetContainerId(), err)
		return nil, err
	}

	err = c.Start()
	if err != nil {
		logger.Errorf("StartContainer: ContainerID %v trying to start container: %v", req.GetContainerId(), err)
		return nil, err
	}

	logger.Infof("StartContainer successful: ContainerID %v", c.ID)

	response := &rtApi.StartContainerResponse{}

	logger.Debugf("StartContainer responded: %v", response)

	return response, nil
}

// StopContainer stops a running container with a grace period (i.e., timeout). This call is idempotent, and must not
// return an error if the container has already been stopped.
func (s RuntimeServer) StopContainer(ctx context.Context, req *rtApi.StopContainerRequest) (*rtApi.StopContainerResponse, error) {
	logger.Infof("StopContainer called: ContainerID %v", req.GetContainerId())
	logger.Debugf("StopContainer triggered: %v", req)

	c, err := s.lxf.GetContainer(req.GetContainerId())
	if err != nil {
		if shared.IsErrNotFound(err) {
			return &rtApi.StopContainerResponse{}, nil
		}

		logger.Errorf("StopContainer: ContainerID %v trying to get container: %v", req.GetContainerId(), err)

		return nil, err
	}

	err = s.stopContainer(c, int(req.Timeout))
	if err != nil {
		logger.Errorf("StopContainer: ContainerID %v trying to stop container: %v", req.GetContainerId(), err)
		return nil, err
	}

	logger.Infof("StopContainer successful: ContainerID %v", c.ID)

	response := &rtApi.StopContainerResponse{}

	logger.Debugf("StopContainer responded: %v", response)

	return response, nil
}

// RemoveContainer removes the container. If the container is running, the container must be forcibly removed. This call
// is idempotent, and must not return an error if the container has already been removed. nolint: dupl
func (s RuntimeServer) RemoveContainer(ctx context.Context, req *rtApi.RemoveContainerRequest) (*rtApi.RemoveContainerResponse, error) {
	logger.Infof("RemoveContainer called: ContainerID %v", req.GetContainerId())
	logger.Debugf("RemoveContainer triggered: %v", req)

	c, err := s.lxf.GetContainer(req.GetContainerId())
	if err != nil {
		if shared.IsErrNotFound(err) {
			return &rtApi.RemoveContainerResponse{}, nil
		}

		logger.Errorf("RemoveContainer: ContainerID %v trying to get container: %v", req.GetContainerId(), err)

		return nil, err
	}

	err = s.deleteContainer(ctx, c)
	if err != nil {
		logger.Errorf("RemoveContainer: ContainerID %v trying to remove container: %v", req.GetContainerId(), err)
		return nil, err
	}

	logger.Infof("RemoveContainer successful: ContainerID %v", c.ID)

	response := &rtApi.RemoveContainerResponse{}

	logger.Debugf("RemoveContainer responded: %v", response)

	return response, nil
}

// ListContainers lists all containers by filters.
func (s RuntimeServer) ListContainers(ctx context.Context, req *rtApi.ListContainersRequest) (*rtApi.ListContainersResponse, error) {
	logger.Debugf("ListContainers triggered: %v", req)

	response := &rtApi.ListContainersResponse{}

	cl, err := s.lxf.ListContainers()
	if err != nil {
		logger.Errorf("ListContainers: trying to get container list: %v", err)
		return nil, err
	}

	for _, c := range cl {
		if req.GetFilter() != nil {
			filter := req.GetFilter()
			if filter.GetId() != "" && filter.GetId() != c.ID {
				continue
			}

			if filter.GetState() != nil && filter.GetState().GetState() != stateContainerAsCri(c.StateName) {
				continue
			}

			if filter.GetPodSandboxId() != "" && filter.GetPodSandboxId() != c.SandboxID() {
				continue
			}

			if !CompareFilterMap(c.Labels, filter.GetLabelSelector()) {
				continue
			}
		}

		response.Containers = append(response.Containers, toCriContainer(c))
	}

	logger.Debugf("ListContainers responded: %v", response)

	return response, nil
}

// ContainerStatus returns status of the container. If the container is not present, returns an error.
func (s RuntimeServer) ContainerStatus(ctx context.Context, req *rtApi.ContainerStatusRequest) (*rtApi.ContainerStatusResponse, error) {
	//logger.Infof("ContainerStatus called: ContainerID %v", req.GetContainerId())
	logger.Debugf("ContainerStatus triggered: %v", req)

	ct, err := s.lxf.GetContainer(req.GetContainerId())
	if err != nil {
		logger.Errorf("ContainerStatus: ContainerID %v trying to get container: %v", req.GetContainerId(), err)
		return nil, err
	}

	response := toCriStatusResponse(ct)

	logger.Debugf("ContainerStatus responded: %v", response)

	return response, nil
}

// UpdateContainerResources updates ContainerConfig of the container.
func (s RuntimeServer) UpdateContainerResources(ctx context.Context, req *rtApi.UpdateContainerResourcesRequest) (*rtApi.UpdateContainerResourcesResponse, error) {
	logger.Debugf("UpdateContainerResources triggered: %v", req)
	return nil, fmt.Errorf("UpdateContainerResources: %w", ErrNotImplemented)
}

// ReopenContainerLog asks runtime to reopen the stdout/stderr log file for the container. This is often called after
// the log file has been rotated. If the container is not running, container runtime can choose to either create a new
// log file and return nil, or return an error. Once it returns error, new container log file MUST NOT be created.
func (s RuntimeServer) ReopenContainerLog(ctx context.Context, req *rtApi.ReopenContainerLogRequest) (*rtApi.ReopenContainerLogResponse, error) {
	logger.Debugf("ReopenContainerLog triggered: %v", req)
	return nil, fmt.Errorf("ReopenContainerLog: %w", ErrNotImplemented)
}

// ExecSync runs a command in a container synchronously.
func (s RuntimeServer) ExecSync(ctx context.Context, req *rtApi.ExecSyncRequest) (*rtApi.ExecSyncResponse, error) {
	logger.Debugf("ExecSync triggered: %v", req)

	stdin := bytes.NewReader(nil)
	stdinR := ioutil.NopCloser(stdin)
	stdout := bytes.NewBuffer(nil)
	stdoutW := ioutils.WriteCloserWrapper(stdout)
	stderr := bytes.NewBuffer(nil)
	stderrW := ioutils.WriteCloserWrapper(stderr)

	code, err := s.lxf.Exec(req.GetContainerId(), req.GetCmd(), stdinR, stdoutW, stderrW, false, false, req.GetTimeout(), nil)

	logger.Debugf("received exit code %v for exec %v on container %v", code, req.GetCmd(), req.GetContainerId())

	return &rtApi.ExecSyncResponse{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: code,
	}, err
}

// Exec prepares a streaming endpoint to execute a command in the container.
func (s RuntimeServer) Exec(ctx context.Context, req *rtApi.ExecRequest) (*rtApi.ExecResponse, error) {
	logger.Debugf("Exec triggered: %v", req)

	resp, err := s.stream.streamServer.GetExec(req)
	if err != nil {
		logger.Errorf("Exec: ContainerID %v preparing exec endpoint: %v", req.GetContainerId(), err)
		return nil, err
	}

	logger.Debugf("Exec responded: %v", resp)

	return resp, nil
}

func (ss streamService) Exec(containerID string, cmd []string, stdinR io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	logger.Debugf("StreamService Exec triggered: {containerID: %v, cmd: %v, stdin: %#v, stdout: %#v, stderr: %#v, tty: %v, resize: %v}", containerID, cmd, stdinR, stdout, stderr, tty, resize)

	var stdin io.ReadCloser
	if stdinR == nil {
		stdin = ioutil.NopCloser(bytes.NewReader(nil))
	} else {
		stdin = ioutil.NopCloser(stdinR)
	}

	interactive := (stdinR != nil)

	code, err := ss.runtimeServer.lxf.Exec(containerID, cmd, stdin, stdout, stderr, interactive, tty, 0, resize)

	logger.Debugf("received exit code %v for exec %v on container %v", code, cmd, containerID)

	if err != nil || code != 0 {
		return &utilExec.CodeExitError{
			Err:  errors.Errorf("error executing command %v, exit code %d, reason %v", cmd, code, err),
			Code: int(code),
		}
	}

	return nil
}

// Attach prepares a streaming endpoint to attach to a running container.
func (s RuntimeServer) Attach(ctx context.Context, req *rtApi.AttachRequest) (*rtApi.AttachResponse, error) {
	logger.Debugf("Attach triggered: %v", req)
	logger.Errorf("Attach - not implemented")

	return nil, fmt.Errorf("Attach: %w", ErrNotImplemented)
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox.
func (s RuntimeServer) PortForward(ctx context.Context, req *rtApi.PortForwardRequest) (resp *rtApi.PortForwardResponse, err error) {
	logger.Debugf("PortForward triggered: %v", req)

	resp, err = s.stream.streamServer.GetPortForward(req)
	if err != nil {
		logger.Errorf("PortForward: preparing pendpoint: %v", err)
		return nil, err
	}

	logger.Debugf("PortForward responded: %v", resp)

	return resp, nil
}

// TODO: extract streamService in own file

func (ss streamService) PortForward(podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	sb, err := ss.runtimeServer.lxf.GetSandbox(podSandboxID)
	if err != nil {
		err = errors.Wrapf(err, "unable to find pod %v", podSandboxID)
		logger.Errorf("%v", err)

		return err
	}

	podIP := ss.runtimeServer.getInetAddress(context.TODO(), sb)

	_, err = exec.LookPath("socat")
	if err != nil {
		err = errors.Wrap(err, "unable to do port forwarding")
		logger.Errorf("%v", err)

		return err
	}

	args := []string{"-", fmt.Sprintf("TCP4:%s:%d,keepalive", podIP, port)}

	commandString := fmt.Sprintf("socat %s", strings.Join(args, " "))
	logger.Debugf("executing port forwarding command: %s", commandString)

	command := exec.Command("socat", args...)
	command.Stdout = stream

	stderr := new(bytes.Buffer)
	command.Stderr = stderr

	// If we use Stdin, command.Run() won't return until the goroutine that's copying from stream finishes. Unfortunately,
	// if you have a client like telnet connected via port forwarding, as long as the user's telnet client is connected to
	// the user's local listener that port forwarding sets up, the telnet session never exits. This means that even if
	// socat has finished running, command.Run() won't ever return (because the client still has the connection and stream
	// open). The work around is to use StdinPipe(), as Wait() (called by Run()) closes the pipe when the command (socat)
	// exits.
	inPipe, err := command.StdinPipe()
	if err != nil {
		logger.Errorf("PortForward: unable to do port forwarding: %v", err)
		return err
	}

	go func() {
		_, err = pools.Copy(inPipe, stream)
		if err != nil {
			logger.Errorf("pipe copy errored: %v", err)
		}

		err = inPipe.Close()
		if err != nil {
			logger.Errorf("pipe close errored: %v", err)
		}
	}()

	if err := command.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}

	return nil
}

// ContainerStats returns stats of the container. If the container does not exist, the call returns an error.
func (s RuntimeServer) ContainerStats(ctx context.Context, req *rtApi.ContainerStatsRequest) (*rtApi.ContainerStatsResponse, error) {
	logger.Debugf("ContainerStats triggered: %v", req)

	response := &rtApi.ContainerStatsResponse{}

	cntStat, err := s.lxf.GetContainer(req.GetContainerId())
	if err != nil {
		logger.Errorf("ContainerStats: ContainerID %v trying to get container: %v", req.GetContainerId(), err)
		return nil, err
	}

	response.Stats, err = toCriStats(cntStat)
	if err != nil {
		logger.Errorf("ContainerStats: ContainerID %v trying to get stats: %v", req.GetContainerId(), err)
		return nil, err
	}

	logger.Debugf("ContainerStats responded: %v", response)

	return response, nil
}

// ListContainerStats returns stats of all running containers.
func (s RuntimeServer) ListContainerStats(ctx context.Context, req *rtApi.ListContainerStatsRequest) (*rtApi.ListContainerStatsResponse, error) {
	logger.Debugf("ListContainerStats triggered: %v", req)

	response := &rtApi.ListContainerStatsResponse{}

	if req.Filter != nil && req.Filter.Id != "" {
		c, err := s.lxf.GetContainer(req.Filter.Id)
		if err != nil {
			logger.Errorf("ListContainerStats: ContainerID %v trying to get container: %v", req.GetFilter().GetId(), err)
			return nil, err
		}

		st, err := toCriStats(c)
		if err != nil {
			logger.Errorf("ListContainerStats: ContainerID %v trying to get stats: %v", req.GetFilter().GetId(), err)
			return nil, err
		}

		response.Stats = append(response.Stats, st)

		return response, nil
	}

	cts, err := s.lxf.ListContainers()
	if err != nil {
		logger.Errorf("ListContainerStats: trying to list containers: %v", err)
		return nil, err
	}

	for _, c := range cts {
		st, err := toCriStats(c)
		if err != nil {
			logger.Errorf("ListContainerStats: ContainerID %v trying to get stats: %v", c.ID, err)
			return nil, err
		}

		response.Stats = append(response.Stats, st)
	}

	logger.Debugf("ListContainerStats responded: %v", response)

	return response, nil
}

// UpdateRuntimeConfig updates the runtime configuration based on the given request.
func (s RuntimeServer) UpdateRuntimeConfig(ctx context.Context, req *rtApi.UpdateRuntimeConfigRequest) (*rtApi.UpdateRuntimeConfigResponse, error) {
	//logger.Infof("UpdateRuntimeConfig called: PodCIDR %v", req.GetRuntimeConfig().GetNetworkConfig().GetPodCidr())
	logger.Debugf("UpdateRuntimeConfig triggered: %v", req)

	err := s.network.UpdateRuntimeConfig(req.GetRuntimeConfig())
	if err != nil {
		logger.Errorf("UpdateRuntimeConfig: %v", err)
		return nil, err
	}

	response := &rtApi.UpdateRuntimeConfigResponse{}

	logger.Debugf("UpdateRuntimeConfig responded: %v", response)

	return response, nil
}

// Status returns the status of the runtime.
func (s RuntimeServer) Status(ctx context.Context, req *rtApi.StatusRequest) (*rtApi.StatusResponse, error) {
	logger.Debugf("Status triggered: %v", req)

	// TODO: actually check services!
	response := &rtApi.StatusResponse{
		Status: &rtApi.RuntimeStatus{
			Conditions: []*rtApi.RuntimeCondition{
				{
					Type:   rtApi.RuntimeReady,
					Status: true,
				},
				{
					Type:   rtApi.NetworkReady,
					Status: true,
				},
			},
		},
	}

	logger.Debugf("Status responded: %v", response)

	return response, nil
}
