# network-device-plugin
Kubelet network device plugin

Very hacky example just to demonstrate technically feasibility on how to solve the plumbing of network interfaces to Pods using CDI (removing the depependency on CDI)
Presented in [SIG Network meeting 20240314](https://www.youtube.com/watch?v=67UzeMEaqnM&list=PL69nYSiGNLP2E8vmnqo5MwPOY25sDWIxb&index=1)
Slides in https://docs.google.com/presentation/d/1pjDCtpdbCSWaqCbBYWgzTxAewOVbMf6rUS5SbjAJAe8/edit?usp=sharing


## Requirements

Kubernetes CDI support: in beta since 1.29, for 1.28 you have to enable the feature gate.

Containerd CDI support, will be enabled by default in version 2.0, for previous versions you have to set this option and restart containerd.
https://github.com/cncf-tags/container-device-interface?tab=readme-ov-file#containerd-configuration

```
grep -q enable_cdi /etc/containerd/config.toml || sed -i '/\[plugins."io.containerd.grpc.v1.cri"\]/a\ \ enable_cdi = true' /etc/containerd/config.toml && systemctl restart containerd
```

## Demo with Kind

Create a kind cluster with kind v0.22.0, kindest/node:v1.29.2

```sh
cat <<EOF | kind create cluster --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri"]
     enable_cdi = true
nodes:
- role: control-plane
- role: worker
- role: worker
EOF
```

Optional: Build your own image (remember to modify the install.yaml manifest)
```
docker build -f Dockerfile -t aojea/netdevice-driver:v0.1.0 .
# you can preload it in the kind cluster
kind load docker-image aojea/netdevice-driver:v0.1.0
```

Install the device plugin:
```
 kubectl apply -f install.yaml
clusterrole.rbac.authorization.k8s.io/netdevice-driver created
clusterrolebinding.rbac.authorization.k8s.io/netdevice-driver created
serviceaccount/netdevice-driver created
daemonset.apps/netdevice-driver created
```

current existing manifest is prepared to expose all interfaces prefixed with `dummy`
```
   - /plugin
        - -interfaces
        - dummy
        - -v
```

Create one dummy interface in one of the nodes
```sh
$ docker exec -it kind-worker bash
root@kind-worker:/# ip link add dummy0 type dummy
root@kind-worker:/# ip addr add 192.168.8.8/32 dev dummy0
root@kind-worker:/# ip link set dummy0 up
root@kind-worker:/# ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: dummy0: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether ea:9f:0f:35:e9:30 brd ff:ff:ff:ff:ff:ff
    inet 192.168.8.8/32 scope global dummy0
       valid_lft forever preferred_lft forever
16: eth0@if17: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default
    link/ether 02:42:c0:a8:08:02 brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 192.168.8.2/24 brd 192.168.8.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 fc00:f853:ccd:e793::2/64 scope global nodad
       valid_lft forever preferred_lft forever
    inet6 fe80::42:c0ff:fea8:802/64 scope link
       valid_lft forever preferred_lft forever

```

It will be presented as a resource on the Node
```
kubectl get nodes kind-worker -o json | jq .status.allocatable
{
  "cpu": "48",
  "ephemeral-storage": "1035659044Ki",
  "hugepages-1Gi": "0",
  "hugepages-2Mi": "0",
  "memory": "198059952Ki",
  "networking.k8s.io/netdevice": "1", <--------------------
  "pods": "110"
}


Now you can request that netdevice specifying it in the pod spec

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test
spec:
  containers:
  - name: nginx
    args: [ "netexec","--http-port=80"]
    image: registry.k8s.io/e2e-test-images/agnhost:2.40
    ports:
    - containerPort: 80
    resources:
      limits:
         networking.k8s.io/netdevice: 1
```

and see how the interface was moved inside the Pod
```
kubectl exec -it test ip a
kubectl exec [POD] [COMMAND] is DEPRECATED and will be removed in a future version. Use kubectl exec [POD] -- [COMMAND] instead.
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0@if3: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default
    link/ether 6e:90:6e:78:1c:ba brd ff:ff:ff:ff:ff:ff link-netnsid 0
    inet 10.244.1.2/24 brd 10.244.1.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 fe80::6c90:6eff:fe78:1cba/64 scope link
       valid_lft forever preferred_lft forever
3: dummy0: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether ea:9f:0f:35:e9:30 brd ff:ff:ff:ff:ff:ff
    inet 192.168.8.8/32 scope global dummy0
       valid_lft forever preferred_lft forever
    inet6 fe80::e89f:fff:fe35:e930/64 scope link
       valid_lft forever preferred_lft forever
```
