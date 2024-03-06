# cat /etc/cni/net.d/10-kindnet.conflist

{
        "cniVersion": "0.3.1",
        "name": "kindnet",
        "plugins": [
        {
                "type": "ptp",
                "ipMasq": false,
                "ipam": {
                        "type": "host-local",
                        "dataDir": "/run/cni-ipam-state",
                        "routes": [


                                { "dst": "::/0" }
                        ],
                        "ranges": [


                                [ { "subnet": "fd00:10:244:1::/64" } ]
                        ]
                }
                ,
                "mtu": 1500

        },
        {
                "type": "portmap",
                "capabilities": {
                        "portMappings": true
                }
        },
        {
                "type": "netdriver",
                "capabilities": {
                        "io.kubernetes.cri.pod-annotations": true # undocumented https://github.com/kubernetes/kubernetes/issues/69882
                }
        }
        ]
}
