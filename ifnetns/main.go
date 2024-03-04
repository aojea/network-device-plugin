package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"

	rspecs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
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
	if len(args) != 1 {
		log.Fatalf("expected only one argument, the name of the interface: %v", args)
	}
	ifName := args[0]
	// Get the network namespace from the runtime configuration
	var state rspecs.State
	var spec rspecs.Spec

	// Get the bundle path from the STATE passed in STDIN
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Printf("unable to read stdin: %v", err)
		os.Exit(0)
	}
	err = json.Unmarshal(data, &state)
	if err != nil {
		log.Printf("unable to unmarshal %s: %v", string(data), err)
		os.Exit(0)
	}
	// Get the runtime SPEC
	config, err := os.ReadFile(filepath.Join(state.Bundle, "config.json"))
	if err != nil {
		log.Printf("unable to read OCI spec at %s: %v", state.Bundle, err)
		os.Exit(0)
	}
	err = json.Unmarshal(config, &spec)
	if err != nil {
		log.Printf("unable to unmarshal %s: %v", string(config), err)
		os.Exit(0)
	}

	if spec.Linux == nil {
		return
	}
	var nsPath string
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == rspecs.NetworkNamespace {
			nsPath = ns.Path
			break
		}
	}
	if nsPath == "" {
		os.Exit(0)
	}

	err = linkSetNS(ifName, nsPath)
	if err != nil {
		log.Printf("error moving the interface to the namespaece: %v", err)
		os.Exit(1)
	}
}

func linkSetNS(ifName, nsPath string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return err
	}
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return err
	}
	defer ns.Close()
	// Devices can be renamed only when down
	err = netlink.LinkSetDown(link)
	if err != nil {
		return err
	}
	// Save host device name into the container device's alias property
	err = netlink.LinkSetAlias(link, link.Attrs().Name)
	if err != nil {
		return fmt.Errorf("fail to set alias for iface %s: %w", ifName, err)
	}
	err = netlink.LinkSetNsFd(link, int(ns))
	if err != nil {
		return fmt.Errorf("fail to move link for iface %s to ns %d : %v", ifName, int(ns), err)
	}
	// This is now inside the container namespace
	err = netns.Set(ns)
	if err != nil {
		return fmt.Errorf("fail to set to ns %d: %v", int(ns), err)
	}
	return nil
}
