binary to be used in an OCI createRuntime hook to pass the network device
interface name , it will get the network namespace from the oci arguments
and move this interface into the container namespace

Remember, network interfaces wipe the configuration when they are moved to
different namespaces
