# CSI Conformance Testing

This document describes how to run CSI conformance tests against the
go-bpfman CSI driver using the official `csi-sanity` tool.

## Overview

The [csi-sanity](https://github.com/kubernetes-csi/csi-test) tool tests CSI
drivers for compliance with the CSI specification. It connects to a running
driver via its Unix socket and exercises the gRPC API.

## Limitations

This CSI driver implements only the **Node** and **Identity** services for
ephemeral inline volumes. It does not implement the **Controller** service
because:

- Volumes are created on-demand when pods reference them
- No external provisioning or volume lifecycle management is needed
- The driver exposes pre-existing BPF maps, not dynamically provisioned storage

As a result, only Identity service tests pass fully. Node service tests fail
because `csi-sanity` expects a Controller service to create volumes before
testing Node operations.

## Prerequisites

Build the `csi-sanity` container image:

```bash
make build-image-csi-sanity
```

## Running Tests

### Deploy the Driver

Ensure the driver is running in your cluster and that the
`csi-sanity:dev` image (built above) is available to the cluster's
node(s) — how you get the image into the cluster depends on your
runtime (image registry pull, local cache, etc.).

### Run Identity Tests

Run the tests as a privileged pod with access to the CSI socket:

```bash
kubectl run csi-sanity --rm -it --restart=Never \
  --image=csi-sanity:dev \
  --overrides='{
    "spec": {
      "containers": [{
        "name": "csi-sanity",
        "image": "csi-sanity:dev",
        "imagePullPolicy": "IfNotPresent",
        "args": [
          "--csi.endpoint", "/var/lib/kubelet/plugins/csi.go-bpfman.io/csi.sock",
          "--ginkgo.focus", "Identity",
          "--ginkgo.v"
        ],
        "volumeMounts": [{
          "name": "plugin-dir",
          "mountPath": "/var/lib/kubelet/plugins/csi.go-bpfman.io"
        }],
        "securityContext": {
          "privileged": true
        }
      }],
      "volumes": [{
        "name": "plugin-dir",
        "hostPath": {
          "path": "/var/lib/kubelet/plugins/csi.go-bpfman.io",
          "type": "Directory"
        }
      }]
    }
  }'
```

Expected output:

```
Running Suite: CSI Driver Test Suite - /
========================================
Random Seed: 1768927850

Will run 3 of 92 specs
------------------------------
Identity Service GetPluginCapabilities should return appropriate capabilities
  STEP: connecting to CSI driver
  STEP: creating mount and staging directories
  STEP: [Node Service] checking successful response
* [0.002 seconds]
------------------------------
Identity Service Probe should return appropriate information
  STEP: reusing connection to CSI driver
  STEP: creating mount and staging directories
  STEP: [Node Service] verifying return status
* [0.000 seconds]
------------------------------
Identity Service GetPluginInfo should return appropriate information
  STEP: reusing connection to CSI driver
  STEP: creating mount and staging directories
  STEP: [Node Service] verifying name size and characters
* [0.000 seconds]

Ran 3 of 92 Specs in 0.003 seconds
SUCCESS! -- 3 Passed | 0 Failed | 1 Pending | 88 Skipped
```

### Run All Applicable Tests

To run all tests except those requiring Controller service, change the args:

```bash
"args": [
  "--csi.endpoint", "/var/lib/kubelet/plugins/csi.go-bpfman.io/csi.sock",
  "--ginkgo.skip", "Controller",
  "--ginkgo.v"
]
```

Note: Node service tests will still fail because `csi-sanity` uses a BeforeEach
hook that calls `ControllerGetCapabilities` to determine how to provision test
volumes.

## Test Results Summary

| Service  | Tests | Status |
|----------|-------|--------|
| Identity | 3     | PASS   |
| Node     | 15    | SKIP (requires Controller) |
| Controller | 74  | SKIP (not implemented) |

## Why Node Tests Fail

The `csi-sanity` tool assumes the standard CSI workflow:

1. Controller creates a volume (`CreateVolume`)
2. Node stages the volume (`NodeStageVolume`)
3. Node publishes the volume (`NodePublishVolume`)

Our driver uses **ephemeral inline volumes** where:

1. Pod spec includes volume attributes inline
2. Kubelet calls `NodePublishVolume` directly
3. No Controller involvement needed

This is a valid CSI pattern (used by secrets-store-csi-driver, etc.) but
`csi-sanity` doesn't have a mode to test it.

## Alternative Testing

For ephemeral inline volume drivers, integration testing via actual pod
deployments is more meaningful than `csi-sanity`.

## References

- [CSI Specification](https://github.com/container-storage-interface/spec)
- [csi-sanity documentation](https://github.com/kubernetes-csi/csi-test/blob/master/cmd/csi-sanity/README.md)
- [Kubernetes CSI Functional Testing](https://kubernetes-csi.github.io/docs/functional-testing.html)
