package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"

	rspecs "github.com/opencontainers/runtime-spec/specs-go"
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

// move the network interfaces passed to the container network namespace
func main() {
	// Lock the OS Thread so we don't accidentally switch namespaces
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	f, err := os.OpenFile("/var/log/oci-debug.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer f.Close()

	log.SetOutput(f)

	log.Printf("ENV: %+v\n", os.Environ())

	// Get the network namespace from the runtime configuration
	var state rspecs.State
	var spec rspecs.Spec

	// Get the bundle path from the STATE passed in STDIN
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Printf("unable to read stdin: %v", err)
		os.Exit(0)
	}
	log.Printf("STDIN: %s\n", string(data))

	err = json.Unmarshal(data, &state)
	if err != nil {
		log.Printf("unable to unmarshal %s: %v", string(data), err)
		os.Exit(0)
	}
	log.Printf("STATUS: %+v\n", state)

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
	log.Printf("CONFIG: %+v\n", spec)
}
