package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
)

// Ref: https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/

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
	kubeletSocket = "kubelet.sock"
	pluginSocket  = "titanium.sock"
	pluginName    = "titanium"
	resourceName  = "networking.gke.io/netdev"
)

var (
	flagRegex string
)

var _ registerapi.RegistrationServer = &plugin{}
var _ pluginapi.DevicePluginServer = &plugin{}

type plugin struct {
	Version      string
	ResourceName string
	Endpoint     string
	Name         string
	Type         string

	s *grpc.Server

	mu            sync.Mutex
	registered    bool
	registerError error
	devices       []string
}

func newPlugin() *plugin {
	return &plugin{
		Version:      "v1beta1",
		ResourceName: resourceName,
		Type:         registerapi.DevicePlugin,
		Endpoint:     path.Join(pluginapi.DevicePluginPath, pluginSocket),
	}
}
func (p *plugin) GetInfo(context.Context, *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	klog.V(3).Infof("GetInfo request")
	return &registerapi.PluginInfo{
		Type: p.Type,
		Name: p.Name,
	}, nil
}

func (p *plugin) NotifyRegistrationStatus(ctx context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	klog.V(3).Infof("NotifyRegistrationStatus request: %v", status)
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
	klog.V(3).Infof("GetPreferredAllocation request: %v", in)
	return &pluginapi.PreferredAllocationResponse{}, nil
}

func (p *plugin) ListAndWatch(_ *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	for {
		ifaces, err := net.Interfaces()
		if err != nil {
			klog.Infof("error getting system interfaces: %v", err)
		}
		response := pluginapi.ListAndWatchResponse{}
		devices := []string{}
		for _, iface := range ifaces {
			// only interested in eth interfaces
			if !strings.Contains(iface.Name, "eth") {
				continue
			}
			// skip default interface
			if iface.Name == "eth0" {
				continue
			}

			health := pluginapi.Unhealthy
			if iface.Flags&net.FlagUp == 1 {
				health = pluginapi.Healthy
				devices = append(p.devices, iface.Name)
			}

			// TODO we can get the driver to discriminate using getIfaceDriver
			response.Devices = append(response.Devices, &pluginapi.Device{
				ID:     iface.Name,
				Health: health,
			})

		}

		if len(response.Devices) > 0 {
			err = s.Send(&response)
			if err != nil {
				return err
			}

			p.mu.Lock()
			p.devices = devices
			p.mu.Unlock()
		}

		time.Sleep(30 * time.Second)
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
	klog.V(3).Infof("Allocate request: %v", in)
	response := &pluginapi.AllocateResponse{}
	for _, request := range in.GetContainerRequests() {
		// Pass the CDI device plugin with annotations or environment variables
		// and add a hook on the CDI plugin that reads this and perform the
		// ip link ethX set netns NS
	}

	return nil, nil
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

// startServer start the plugin server
func (p *plugin) start() error {
	socket, err := net.Listen("unix", p.Endpoint)
	if err != nil {
		return err
	}

	p.s = grpc.NewServer()
	registerapi.RegisterRegistrationServer(p.s, p)

	err = p.s.Serve(socket)
	if err != nil {
		return err
	}

	return nil
}

func (p *plugin) stop() error {
	p.s.GracefulStop()

	if err := os.Remove(p.Endpoint); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// register the plugin in the kubelet
func (p *plugin) register() error {
	ctx, timeoutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer timeoutCancel()

	conn, err := grpc.DialContext(ctx, "unix://"+path.Join(pluginapi.DevicePluginPath, kubeletSocket), grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect %s, %v", path.Join(pluginapi.DevicePluginPath, kubeletSocket), err)
	}
	defer conn.Close()
	client := pluginapi.NewRegistrationClient(conn)

	_, err = client.Register(context.Background(), &pluginapi.RegisterRequest{
		Version:      p.Version,
		Endpoint:     p.Endpoint,
		ResourceName: p.ResourceName,
		Options: &pluginapi.DevicePluginOptions{
			PreStartRequired: true,
		},
	})
	if err != nil {
		klog.Errorf("%s: Registration failed: %v", p.Name, err)
		return err
	}
	// wait until kubelet notifies is correctly registered
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.registered, p.registerError
	})
	return err
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
	// Parse command line flags and arguments
	flag.Parse()
	args := flag.Args()

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
	go func() {
		select {
		case <-signalCh:
			klog.Info("Exiting: received signal")
			cancel()
		case <-ctx.Done():
		}
	}()

	if len(args) != 2 {
		flag.Usage()
		os.Exit(1)
	}

	_, err := os.Stat(pluginapi.DevicePluginPath)
	if err != nil {
		klog.Fatalf("kubelet plugin path %s does not exist: %v", pluginapi.DevicePluginPath, err)
	}

	plugin := newPlugin()

	// validate flags
	if flagRegex != "" {
		r, err := regexp.Compile(flagRegex)
		if err != nil {
			klog.Fatalf("flag regex is not a valid regular expression: %v", err)
		}
		plugin.regex = r
	}

	// Note: The ordering of the workflow is important.
	// A plugin MUST start serving gRPC service before registering itself with kubelet for successful registration.
	go plugin.start()
	defer plugin.stop()

	err = plugin.register()
	if err != nil {
		klog.Fatalf("kubelet plugin %s failed to register: %v", plugin.Name, err)
	}

	os.Exit(0)

}
