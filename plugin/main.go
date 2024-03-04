package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
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
	pluginSocket  = "netdevice.sock"
	pluginName    = "netdevice"
	resourceName  = "networking.k8s.io/netdevice"
	cdiPath       = "/var/run/cdi"
	cdiBinPath    = "/opt/cdi/bin"
)

var (
	flagRegex string
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
	cdi.Version = specs.CurrentVersion // TODO to understand what is the minimum version supported in containerd, using 0.5 for safety
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

	for {
		ifaces, err := net.Interfaces()
		if err != nil {
			klog.Infof("error getting system interfaces: %v", err)
		}
		response := pluginapi.ListAndWatchResponse{}
		devices := []netdevice{}
		for _, iface := range ifaces {
			klog.V(2).Infof("Checking iface %s", iface.Name)
			// skip default interface
			if iface.Name == p.gwIface {
				continue
			}
			// only interested in interfaces that match the regex
			if p.regex != nil && !p.regex.MatchString(iface.Name) {
				continue
			}

			if iface.Flags&net.FlagLoopback == 1 {
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

			health := pluginapi.Unhealthy
			if iface.Flags&net.FlagUp == 1 {
				health = pluginapi.Healthy
			}

			// TODO we can get the driver to discriminate using getIfaceDriver
			response.Devices = append(response.Devices, &pluginapi.Device{
				ID:     iface.Name,
				Health: health,
			})

		}

		klog.V(2).Infof("Found following ifaces %v", devices)
		if len(response.Devices) > 0 {
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
		}

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

func getIfaceDriver(name string) (string, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_IP)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)

	info, err := unix.IoctlGetEthtoolDrvinfo(fd, name)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimRight(info.Driver[:], "\x00")), nil
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
			if len(p.devices) == 0 {
				return nil, fmt.Errorf("requested devices are not available %q", id)
			}
			// pop the first device
			name := resourceName + "=" + p.devices[0].Name
			p.devices = p.devices[1:]
			resp.CDIDevices = append(resp.CDIDevices, &pluginapi.CDIDevice{Name: name})
			klog.V(2).Infof("Allocate request interface: %s", name)

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
	if err := os.Remove(p.Endpoint); err != nil && !os.IsNotExist(err) {
		return err
	}

	socket, err := net.Listen("unix", p.Endpoint)
	if err != nil {
		return err
	}
	// Allow 5 sec gracefull shutdown
	defer func() {
		time.Sleep(5 * time.Second)
		socket.Close()
		os.Remove(p.Endpoint)
	}()

	p.s = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(p.s, p)

	go func() {
		defer p.s.Stop()
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
		return err
	}
	<-ctx.Done()
	return nil
}

// register the plugin in the kubelet
func (p *plugin) register(ctx context.Context) error {
	ctx, timeoutCancel := context.WithTimeout(ctx, 35*time.Second)
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

	/* This does not seem to work
	// wait until kubelet notifies is correctly registered
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.registered, p.registerError
	})
	return err
	*/
	return nil
}

func init() {
	klog.InitFlags(nil)
	flag.StringVar(&flagRegex, "interfaces", "", "regex matching the network interfaces used for allocations")

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

	// validate flags
	if flagRegex != "" {
		r, err := regexp.Compile(flagRegex)
		if err != nil {
			klog.Fatalf("flag regex is not a valid regular expression: %v", err)
		}
		plugin.regex = r
	}

	klog.Info("get default gateway interface")
	plugin.gwIface, err = getDefaultGwIf()
	if err != nil {
		klog.Fatalf("kubelet plugin %s failed to find default interface: %v", plugin.Name, err)
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

	// kubelet when restarts remove all the sockets
	// so we need to detect restarts
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		klog.Fatalf("Unable to create fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	err = watcher.Add(plugin.Endpoint)
	if err != nil {
		klog.Fatalf("Unable to add device plugin socket path to fsnotify watcher: %v", err)
	}

	klog.Info("start plugin")
	go plugin.run(ctx)

	for {
		select {
		case <-signalCh:
			klog.Info("Exiting: received signal")
			cancel()
		case <-ctx.Done():
			klog.Info("Exiting: context cancelled")
		case <-watcher.Events:
			// TODO: improve this, currently check if does not exist to restart the plugin
			_, err = os.Stat(plugin.Endpoint)
			if err != nil && os.IsNotExist(err) {
				klog.Info("restart plugin")
				go plugin.run(ctx)
			}
		}
	}
}

func getDefaultGwIf() (string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", err
	}

	for _, r := range routes {
		// no multipath
		if len(r.MultiPath) == 0 {
			if r.Gw == nil {
				continue
			}
			intfLink, err := netlink.LinkByIndex(r.LinkIndex)
			if err != nil {
				log.Printf("Failed to get interface link for route %v : %v", r, err)
				continue
			}
			return intfLink.Attrs().Name, nil
		}

		// multipath, use the first valid entry
		// xref: https://github.com/vishvananda/netlink/blob/6ffafa9fc19b848776f4fd608c4ad09509aaacb4/route.go#L137-L145
		for _, nh := range r.MultiPath {
			if nh.Gw == nil {
				continue
			}
			intfLink, err := netlink.LinkByIndex(r.LinkIndex)
			if err != nil {
				log.Printf("Failed to get interface link for route %v : %v", r, err)
				continue
			}
			return intfLink.Attrs().Name, nil
		}
	}
	return "", fmt.Errorf("not routes found")
}
