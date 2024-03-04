package main

import (
	"log"
	"os"
	"runtime"

	"github.com/vishvananda/netlink"
)

// OCI Hooks
// https://github.com/opencontainers/runtime-spec/blob/master/config.md#createcontainer-hooks

// CreateRuntime
// The createRuntime hooks' path MUST resolve in the runtime namespace.
// The createRuntime hooks MUST be executed in the runtime namespace.

// CreateContainer
// The createContainer hooks' path MUST resolve in the runtime namespace.
// The createContainer hooks MUST be executed in the container namespace.

// OCI state
// The state of the container MUST be passed to hooks over stdin
// so that they may do work appropriate to the current state of the container
// The bundle represents the dir path to container filesystem,
// container runtime state is passed to the hook's stdin
// https://github.com/opencontainers/runtime-spec/blob/master/runtime.md#state

// OCI config
// https://github.com/opencontainers/runtime-spec/blob/main/config.md

// move the network interface passed as argument to the container network namespace
// the first argument is the interface name and the rest are addresses in netlink.Addr.String() format
func main() {
	// Lock the OS Thread so we don't accidentally switch namespaces
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	f, err := os.OpenFile("/var/log/oci.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer f.Close()
	log.SetOutput(f)

	args := os.Args

	if len(args) == 0 {
		log.Fatalf("expected at least one argument, the name of the interface: %v", args)
	}
	ifName := args[0]
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		log.Fatalf("can not get interface %s by name: %v", ifName, err)
	}

	if len(args) > 1 {
		for _, addr := range args[1:] {
			nlAddr, err := netlink.ParseAddr(addr)
			if err != nil {
				log.Printf("error parsing address %s: %v", addr, err)
				continue
			}
			err = netlink.AddrAdd(link, nlAddr)
			if err != nil {
				log.Printf("error adding address %s: %v", addr, err)
				continue
			}
		}
	}

	// Bring container device up
	err = netlink.LinkSetUp(link)
	if err != nil {
		log.Fatalf("can not set interface %s up: %v", ifName, err)
	}

}
