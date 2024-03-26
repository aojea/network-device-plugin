# network-device-plugin
Kubelet network device plugin


## Requirements

Kubernetes has CDI support in beta since 1.29, for 1.28 you have to enable the feature gate.

Containerd CDI will be enabled by default in version 2.0, for previous versions you have to set this option and restart containerd
https://github.com/cncf-tags/container-device-interface?tab=readme-ov-file#containerd-configuration

```
grep -q enable_cdi /etc/containerd/config.toml || sed -i '/\[plugins."io.containerd.grpc.v1.cri"\]/a\ \ enable_cdi = true' /etc/containerd/config.toml && systemctl restart containerd
```

