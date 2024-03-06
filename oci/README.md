

oci is a binary that can read and interpret the information passed in an OCI
hook and print it to a log file, useful for debugging


containerd config.toml

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  # set default runtime handler to v2, which has a per-pod shim
  runtime_type = "io.containerd.runc.v2"
  # Generated by "ctr oci spec" and modified at base container to mount poduct_uuid
  base_runtime_spec = "/etc/containerd/cri-base.json"


ctr oci spec > cri-base.json

  "hooks": {
    "createContainer": [
      {
        "path": "/kind/bin/mount-product-files.sh"
      },
      {
        "path": "/oci"
      }
    ]
  }
}
