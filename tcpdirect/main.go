package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path"
	"regexp"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"tags.cncf.io/container-device-interface/pkg/cdi"
	"tags.cncf.io/container-device-interface/specs-go"
)

//
// The kubelet exports a Registration gRPC service:
// service Registration {
// 	rpc Register(RegisterRequest) returns (Empty) {}
// }
// A device plugin can register itself with the kubelet through this gRPC service. During the registration, the device plugin needs to send:
// The name of its Unix socket.
// The Device Plugin API version against which it was built.
// The ResourceName it wants to advertise. Here ResourceName needs to follow the extended resource naming scheme as vendor-domain/resourcetype. (For example, an NVIDIA GPU is advertised as nvidia.com/gpu.)

const (
	// https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/
	kubeletSocket = "kubelet.sock"
	pluginSocket  = "tcpdirect.sock"
	pluginName    = "tcpdirect"
	resourceName  = "networking.k8s.io/tcpdirect"
	cdiPath       = "/var/run/cdi"
	cdiBinPath    = "/var/run/cdi/bin"
)

// https://man7.org/linux/man-pages/man7/netdevice.7.html
type netdevice struct {
	Name      string
	Addresses []string // IP/Mask format
	MTU       int
}

var _ registerapi.RegistrationServer = &plugin{}
var _ pluginapi.DevicePluginServer = &plugin{}

type plugin struct {
	Version      string
	ResourceName string
	Endpoint     string
	Name         string
	Type         string

	s        *grpc.Server
	registry cdi.Registry

	mu            sync.Mutex
	registered    bool
	registerError error
	devices       []netdevice
	regex         *regexp.Regexp
	gwIface       string
}

func newCDISpec() *specs.Spec {
	cdi := &specs.Spec{}
	cdi.Version = "0.5.0" // GKE compatible version
	cdi.Kind = resourceName
	return cdi
}

func newPlugin() *plugin {
	// https://github.com/cncf-tags/container-device-interface/blob/main/SPEC.md
	return &plugin{
		Version:      pluginapi.Version,
		ResourceName: resourceName,
		Type:         registerapi.DevicePlugin,
		Endpoint:     path.Join(pluginapi.DevicePluginPath, pluginSocket),
		registry:     cdi.GetRegistry(cdi.WithSpecDirs(cdiPath)),
	}
}
func (p *plugin) GetInfo(context.Context, *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	klog.V(2).Infof("GetInfo request")
	return &registerapi.PluginInfo{
		Type: p.Type,
		Name: p.Name,
	}, nil
}

func (p *plugin) NotifyRegistrationStatus(ctx context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	klog.V(2).Infof("NotifyRegistrationStatus request: %v", status)
	p.mu.Lock()
	defer p.mu.Unlock()
	if status.PluginRegistered {
		klog.Infof("%s gets registered successfully at Kubelet \n", pluginName)
		p.registered = true
		p.registerError = nil
	} else {
		klog.Infof("%s failed to be registered at Kubelet: %v; restarting.\n", pluginName, status.Error)
		p.registered = false
		p.registerError = fmt.Errorf(status.Error)
	}
	return &registerapi.RegistrationStatusResponse{}, nil
}

func (p *plugin) GetPreferredAllocation(ctx context.Context, in *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	klog.V(2).Infof("GetPreferredAllocation request: %v", in)
	return &pluginapi.PreferredAllocationResponse{}, nil
}

func (p *plugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	klog.V(2).Infof("ListAndWatch request")
	nlChannel := make(chan netlink.LinkUpdate)
	doneCh := make(chan struct{})
	defer close(doneCh)
	if err := netlink.LinkSubscribe(nlChannel, doneCh); err != nil {
		klog.Infof("error subscring to netlink interfaces: %v", err)
	}

	// A3 machines has always the same interfaces
	tcpdirectInterfaces := sets.New[string]("eth1", "eth2", "eth3", "eth4")
	for {
		ifaces, err := net.Interfaces()
		if err != nil {
			klog.Infof("error getting system interfaces: %v", err)
		}
		response := pluginapi.ListAndWatchResponse{}
		devices := []netdevice{}
		for _, iface := range ifaces {
			klog.V(2).Infof("Checking iface %s", iface.Name)
			// skip non tcp direct interfaces
			if !tcpdirectInterfaces.Has(iface.Name) {
				continue
			}
			link, err := netlink.LinkByName(iface.Name)
			if err != nil {
				klog.Warningf("Error getting link by name %v", err)
				continue
			}
			addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			if err != nil {
				klog.Warningf("Error getting addresses by link %v", err)
				continue
			}
			netdev := netdevice{
				Name: iface.Name,
				MTU:  iface.MTU,
			}

			for _, addr := range addrs {
				netdev.Addresses = append(netdev.Addresses, addr.String())
			}
			devices = append(devices, netdev)

		}

		health := pluginapi.Healthy
		if len(devices) != len(tcpdirectInterfaces) {
			health = pluginapi.Unhealthy
		}

		// TODO we can get the driver to discriminate using getIfaceDriver
		response.Devices = []*pluginapi.Device{&pluginapi.Device{
			ID:     pluginName,
			Health: health,
		}}

		klog.V(2).Infof("Found following ifaces %v", devices)
		p.mu.Lock()
		// generate cdi config
		cdiSpec := newCDISpec()
		for _, netdev := range devices {
			cdiSpec.Devices = append(cdiSpec.Devices, specs.Device{
				Name: netdev.Name,
				ContainerEdits: specs.ContainerEdits{
					Hooks: []*specs.Hook{
						{ // move from runtime ns to container ns
							HookName: "createRuntime",
							Path:     path.Join(cdiBinPath, "ifnetns"),
							Args:     []string{netdev.Name},
						},
						{ // set interface up and TODO IP addresses
							HookName: "createContainer",
							Path:     path.Join(cdiBinPath, "ifup"),
							Args:     append([]string{netdev.Name}, netdev.Addresses...),
						},
					},
				},
			})
		}

		specName, err := cdi.GenerateNameForSpec(cdiSpec)
		if err != nil {
			klog.V(2).Infof("failed to generate Spec name: %w", err)
			continue
		}

		err = p.registry.SpecDB().WriteSpec(cdiSpec, specName)
		if err != nil {
			klog.V(2).Infof("failed to write Spec name: %w", err)
			continue
		}

		klog.V(2).InfoS("Created CDI file", "path", cdiPath, "devices", devices)

		// update kubelet
		err = s.Send(&response)
		if err != nil {
			klog.V(2).Infof("Error sending message %v", err)
			continue
		}

		// update local cache
		p.devices = devices
		p.mu.Unlock()

		timeout := time.After(time.Minute)
		select {
		// trigger a reconcile
		case <-nlChannel:
			// poor rate limited
			time.Sleep(2 * time.Second)
			// drain the channel
			for len(nlChannel) > 0 {
				<-nlChannel
			}
		case <-timeout:
		}

	}

	return nil
}

// Allocate which return list of devices.
func (p *plugin) Allocate(ctx context.Context, in *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.V(2).Infof("Allocate request: %v", in)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := &v1beta1.AllocateResponse{
		ContainerResponses: make([]*v1beta1.ContainerAllocateResponse, 0, len(in.ContainerRequests)),
	}
	for _, request := range in.GetContainerRequests() {
		// Pass the CDI device plugin with annotations or environment variables
		// and add a hook on the CDI plugin that reads this and perform the
		// ip link ethX set netns NS
		resp := v1beta1.ContainerAllocateResponse{}
		for _, id := range request.DevicesIDs {
			if len(p.devices) != 4 {
				return nil, fmt.Errorf("requested devices are not available %q", id)
			}
			for _, device := range p.devices {
				name := resourceName + "=" + device.Name
				klog.V(2).Infof("Allocating interface: %s", name)
				resp.CDIDevices = append(resp.CDIDevices, &pluginapi.CDIDevice{Name: name})
			}
			klog.V(2).Infof("Allocate request interface: %s", pluginName)

		}
		out.ContainerResponses = append(out.ContainerResponses, &resp)
	}
	klog.V(2).Infof("Allocate request response: %v", out)
	return out, nil
}

// GetDevicePluginOptions returns options to be communicated with Device Manager
func (p *plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (
	*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase
func (p *plugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (
	*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (p *plugin) run(ctx context.Context) error {
	socket, err := net.Listen("unix", p.Endpoint)
	if err != nil {
		return err
	}

	p.s = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(p.s, p)

	go func() {
		err = p.s.Serve(socket)
		if err != nil {
			klog.Infof("Server stopped listening: %v", err)
		}
	}()

	// wait until grpc server is ready
	for i := 0; i < 10; i++ {
		services := p.s.GetServiceInfo()
		if len(services) >= 1 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	klog.Infof("Server is ready listening on: %s", socket.Addr().String())
	// register the plugin
	err = p.register(ctx)
	if err != nil {
		p.s.Stop()
		socket.Close()
		return err
	}

	// Cleanup if socket is cancelled
	go func() {
		<-ctx.Done()
		p.s.Stop()
		socket.Close()
	}()
	return nil
}

// register the plugin in the kubelet
func (p *plugin) register(ctx context.Context) error {
	ctx, timeoutCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer timeoutCancel()

	conn, err := grpc.DialContext(ctx, "unix://"+path.Join(pluginapi.DevicePluginPath, kubeletSocket), grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect %s, %v", path.Join(pluginapi.DevicePluginPath, kubeletSocket), err)
	}
	defer conn.Close()
	klog.Info("connected to the kubelet")

	client := pluginapi.NewRegistrationClient(conn)
	_, err = client.Register(ctx, &pluginapi.RegisterRequest{
		Version:      p.Version,
		Endpoint:     pluginSocket,
		ResourceName: p.ResourceName,
		Options: &pluginapi.DevicePluginOptions{
			PreStartRequired: false,
		},
	})
	if err != nil {
		klog.Errorf("%s: Registration failed: %v", p.Name, err)
		return err
	}
	klog.Info("connected to the kubelet")

	return nil
}

func init() {
	klog.InitFlags(nil)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: network-device-plugin [options]\n\n")
		flag.PrintDefaults()
	}
}

func main() {
	// flags
	// Parse command line flags and arguments
	flag.Parse()

	_, err := os.Stat(pluginapi.DevicePluginPath)
	if err != nil {
		klog.Fatalf("kubelet plugin path %s does not exist: %v", pluginapi.DevicePluginPath, err)
	}

	klog.Info("initializing plugin")
	plugin := newPlugin()

	if len(cdi.GetRegistry().GetErrors()) > 0 {
		klog.Fatalf("CDI registry errors %v", cdi.GetRegistry().GetErrors())
	}

	// trap Ctrl+C and call cancel on the context
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	// Enable signal handler
	signalCh := make(chan os.Signal, 2)
	defer func() {
		close(signalCh)
		cancel()
	}()
	signal.Notify(signalCh, os.Interrupt, unix.SIGINT)

	if err := os.Remove(plugin.Endpoint); err != nil && !os.IsNotExist(err) {
		klog.Info("error removing the plugin unix socket %s", plugin.Endpoint)
	}
	klog.Info("start plugin")
	ctxPlugin, cancelPlugin := context.WithCancel(ctx)
	err = plugin.run(ctxPlugin)
	if err != nil {
		klog.Fatalf("Unable to start plugin: %v", err)
	}

	ticker := time.NewTicker(time.Second * 15)
	defer ticker.Stop()
	for {
		select {
		case <-signalCh:
			klog.Info("Exiting: received signal")
			cancel()
		case <-ctx.Done():
			klog.Info("Exiting: context cancelled")
		case <-ticker.C:
			// check if socket exists to detect kubelet restarts
			_, err = os.Stat(plugin.Endpoint)
			if err != nil && os.IsNotExist(err) {
				klog.Info("restart plugin")
				cancelPlugin()
				ctxPlugin, cancelPlugin = context.WithCancel(ctx)
				err = plugin.run(ctxPlugin)
				if err != nil {
					klog.Fatalf("Unable to start plugin: %v", err)
				}
			}
		}
	}
}
