package main

// from https://github.com/containernetworking/plugins/blob/main/plugins/main/host-device/host-device.go

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"

	"github.com/vishvananda/netlink"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

const (
	// containerd annotation
	NetInterfacesAnnotation = "networking.k8s.io/interfaces"
)

// https://docs.kernel.org/networking/netdevices.html

// NetConf for host-device config, look the README to learn how to use those parameters
type NetConf struct {
	types.NetConf
	RuntimeConfig struct {
		PodAnnotations map[string]string `json:"io.kubernetes.cri.pod-annotations,omitempty"`
	} `json:"runtimeConfig,omitempty"`
}

var f *os.File

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
	var err error
	f, err = os.OpenFile("/tmp/cni.log",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
	}
}

func loadConf(bytes []byte) (*NetConf, *current.Result, error) {
	conf := &NetConf{}
	var err error
	if err = json.Unmarshal(bytes, conf); err != nil {
		return nil, nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	// Parse previous result.
	if conf.RawPrevResult == nil {
		// return early if there was no previous result, which is allowed for DEL calls
		return conf, &current.Result{}, nil
	}

	// Parse previous result.
	var result *current.Result
	if err = version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	result, err = current.NewResultFromResult(conf.PrevResult)
	if err != nil {
		return nil, nil, fmt.Errorf("could not convert result to current version: %v", err)
	}

	return conf, result, nil
}

func loadInterfaces(bytes []byte) ([]Ifreq, error) {
	n := []Ifreq{}
	var err error
	if err = json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	return n, nil
}

// https://man7.org/linux/man-pages/man7/netdevice.7.html
type Ifreq struct {
	Name      string `json:"name"`
	Address   string `json:"address,omitempty"`
	Broadcast string `json:"broadcast,omitempty"`
	Netmask   string `json:"netmask,omitempty"`
	HWAddr    string `json:"hwaddr,omitempty"`
	Flags     byte   `json:"flags,omitempty"`
	Ifindex   int    `json:"ifindex,omitempty"`
	MTU       int    `json:"mtu,omitempty"`
}

func cmdAdd(args *skel.CmdArgs) error {
	fmt.Fprintf(f, "CMD ADD")
	cfg, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	if cfg.PrevResult == nil {
		return fmt.Errorf("missing prevResult from earlier plugin")
	}

	if len(cfg.RuntimeConfig.PodAnnotations) == 0 {
		return types.PrintResult(cfg.PrevResult, cfg.CNIVersion)
	}
	// fmt.Fprintf(f,"ADD received config %#v\n", cfg)

	kni, ok := cfg.RuntimeConfig.PodAnnotations[NetInterfacesAnnotation]
	if !ok {
		fmt.Fprintf(f, "ADD not annotations config %v\n", cfg.RuntimeConfig.PodAnnotations)
		return types.PrintResult(cfg.PrevResult, cfg.CNIVersion)
	}
	fmt.Fprintf(f, "ADD annotations %v\n", kni)

	interfaces, err := loadInterfaces([]byte(kni))
	if err != nil {
		fmt.Fprintf(f, "error trying to get the interfaces %#v\n", kni)
		// return err
	}

	fmt.Fprintf(f, "received interfaces %#v\n", interfaces)

	containerNs, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer containerNs.Close()

	for _, iface := range interfaces {
		var contDev netlink.Link
		hostDev, err := getLink(iface.Name, iface.HWAddr)
		if err != nil {
			return fmt.Errorf("failed to find host device: %v", err)
		}

		contDev, err = moveLinkIn(hostDev, containerNs, args.IfName)
		if err != nil {
			return fmt.Errorf("failed to move link %v", err)
		}

		result.Interfaces = append(result.Interfaces, &current.Interface{
			Name:    contDev.Attrs().Name,
			Mac:     contDev.Attrs().HardwareAddr.String(),
			Sandbox: containerNs.Path(),
		})
	}

	return types.PrintResult(result, cfg.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	fmt.Fprintf(f, "----------- CMD DEL\n")
	cfg, _, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	fmt.Fprintf(f, "DEL received config %#v\n", cfg)
	if args.Netns == "" {
		return nil
	}
	containerNs, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer containerNs.Close()

	if cfg.IPAM.Type != "" {
		if err := ipam.ExecDel(cfg.IPAM.Type, args.StdinData); err != nil {
			return err
		}
	}

	// if err := moveLinkOut(containerNs, args.IfName); err != nil {
	//	return err
	//}

	return nil
}

func moveLinkIn(hostDev netlink.Link, containerNs ns.NetNS, ifName string) (netlink.Link, error) {
	if err := netlink.LinkSetNsFd(hostDev, int(containerNs.Fd())); err != nil {
		return nil, err
	}

	var contDev netlink.Link
	if err := containerNs.Do(func(_ ns.NetNS) error {
		var err error
		contDev, err = netlink.LinkByName(hostDev.Attrs().Name)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", hostDev.Attrs().Name, err)
		}
		// Devices can be renamed only when down
		if err = netlink.LinkSetDown(contDev); err != nil {
			return fmt.Errorf("failed to set %q down: %v", hostDev.Attrs().Name, err)
		}
		// Save host device name into the container device's alias property
		if err := netlink.LinkSetAlias(contDev, hostDev.Attrs().Name); err != nil {
			return fmt.Errorf("failed to set alias to %q: %v", hostDev.Attrs().Name, err)
		}
		// Rename container device to respect args.IfName
		if err := netlink.LinkSetName(contDev, ifName); err != nil {
			return fmt.Errorf("failed to rename device %q to %q: %v", hostDev.Attrs().Name, ifName, err)
		}
		// Bring container device up
		if err = netlink.LinkSetUp(contDev); err != nil {
			return fmt.Errorf("failed to set %q up: %v", ifName, err)
		}
		// Retrieve link again to get up-to-date name and attributes
		contDev, err = netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", ifName, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return contDev, nil
}

func moveLinkOut(containerNs ns.NetNS, ifName string) error {
	defaultNs, err := ns.GetCurrentNS()
	if err != nil {
		return err
	}
	defer defaultNs.Close()

	return containerNs.Do(func(_ ns.NetNS) error {
		dev, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", ifName, err)
		}

		// Devices can be renamed only when down
		if err = netlink.LinkSetDown(dev); err != nil {
			return fmt.Errorf("failed to set %q down: %v", ifName, err)
		}

		defer func() {
			// If moving the device to the host namespace fails, set its name back to ifName so that this
			// function can be retried. Also bring the device back up, unless it was already down before.
			if err != nil {
				_ = netlink.LinkSetName(dev, ifName)
				if dev.Attrs().Flags&net.FlagUp == net.FlagUp {
					_ = netlink.LinkSetUp(dev)
				}
			}
		}()

		// Rename the device to its original name from the host namespace
		if err = netlink.LinkSetName(dev, dev.Attrs().Alias); err != nil {
			return fmt.Errorf("failed to restore %q to original name %q: %v", ifName, dev.Attrs().Alias, err)
		}

		if err = netlink.LinkSetNsFd(dev, int(defaultNs.Fd())); err != nil {
			return fmt.Errorf("failed to move %q to host netns: %v", dev.Attrs().Alias, err)
		}
		return nil
	})
}

func printLink(dev netlink.Link, cniVersion string, containerNs ns.NetNS) error {
	result := current.Result{
		CNIVersion: current.ImplementedSpecVersion,
		Interfaces: []*current.Interface{
			{
				Name:    dev.Attrs().Name,
				Mac:     dev.Attrs().HardwareAddr.String(),
				Sandbox: containerNs.Path(),
			},
		},
	}
	return types.PrintResult(&result, cniVersion)
}

func getLink(devname, hwaddr string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list node links: %v", err)
	}
	switch {

	case len(devname) > 0:
		return netlink.LinkByName(devname)
	case len(hwaddr) > 0:
		hwAddr, err := net.ParseMAC(hwaddr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse MAC address %q: %v", hwaddr, err)
		}

		for _, link := range links {
			if bytes.Equal(link.Attrs().HardwareAddr, hwAddr) {
				return link, nil
			}
		}
	}

	return nil, fmt.Errorf("failed to find physical interface")
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("host-device"))
}

func cmdCheck(args *skel.CmdArgs) error {
	cfg, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	// run the IPAM plugin and get back the config to apply
	if cfg.IPAM.Type != "" {
		err = ipam.ExecCheck(cfg.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	// Parse previous result.
	if cfg.NetConf.RawPrevResult == nil {
		return fmt.Errorf("Required prevResult missing")
	}

	if err := version.ParsePrevResult(&cfg.NetConf); err != nil {
		return err
	}

	var contMap current.Interface
	// Find interfaces for name we know, that of host-device inside container
	for _, intf := range result.Interfaces {
		if args.IfName == intf.Name {
			if args.Netns == intf.Sandbox {
				contMap = *intf
				continue
			}
		}
	}

	// The namespace must be the same as what was configured
	if args.Netns != contMap.Sandbox {
		return fmt.Errorf("Sandbox in prevResult %s doesn't match configured netns: %s",
			contMap.Sandbox, args.Netns)
	}

	//
	// Check prevResults for ips, routes and dns against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {
		// Check interface against values found in the container
		err := validateCniContainerInterface(contMap)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedInterfaceIPs(args.IfName, result.IPs)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedRoute(result.Routes)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	//
	return nil
}

func validateCniContainerInterface(intf current.Interface) error {
	var link netlink.Link
	var err error

	if intf.Name == "" {
		return fmt.Errorf("Container interface name missing in prevResult: %v", intf.Name)
	}
	link, err = netlink.LinkByName(intf.Name)
	if err != nil {
		return fmt.Errorf("Container Interface name in prevResult: %s not found", intf.Name)
	}
	if intf.Sandbox == "" {
		return fmt.Errorf("Error: Container interface %s should not be in host namespace", link.Attrs().Name)
	}

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return fmt.Errorf("Interface %s Mac %s doesn't match container Mac: %s", intf.Name, intf.Mac, link.Attrs().HardwareAddr)
		}
	}

	return nil
}
