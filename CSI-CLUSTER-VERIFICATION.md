# Verifying the CSI node driver on a KIND cluster

A runbook for exercising the bpfman CSI node driver end-to-end on a live
KIND cluster: load an example, see the parameters kubelet sends, inspect
how the mounts are built on the node, prove the publish path is
idempotent, then unload and confirm there is no residue.

Everything here is read-only against your cluster except three explicit
mutations: `apply -k`, `delete -k`, and one `systemctl restart kubelet`
(used to make kubelet re-publish so idempotency can be observed).

## Assumptions

- A KIND cluster is up and `kubectl config current-context` points at it.
- `bpfman-daemon` is deployed and is running *this branch's* build. The
  CSI node plugin runs inside the `bpfman` container of that DaemonSet,
  not the `node-driver-registrar` sidecar.
- The host has `docker`, `findmnt`, and `kubectl`. The KIND node's
  Kubernetes name and its docker container name are identical, so the
  same `$NODE` value works for both `kubectl` and `docker exec`.
- The bpfman examples are checked out at
  `~/src/github.com/bpfman/bpfman/worktrees/general/examples`.

## Pick an example (the only knobs)

The `go-<type>-counter` examples all follow the same shape. Set these
five variables and the rest of the runbook is example-agnostic. Defaults
target the XDP counter; the commented lines show how to switch.

```bash
EXAMPLE_DIR=~/src/github.com/bpfman/bpfman/worktrees/general/examples/config/default/go-xdp-counter
APP_NS=go-xdp-counter            # namespace the example creates
APP_LABEL='name=go-xdp-counter'  # pod label selector
VOL_NAME=go-xdp-counter-maps     # volume name in the pod spec (part of the target path)
MAP=xdp_stats_map                # map the example asks for
BPFMAN_NS=bpfman                 # namespace of bpfman-daemon

# kprobe: .../go-kprobe-counter  go-kprobe-counter  name=go-kprobe-counter  go-kprobe-counter-maps  kprobe_stats_map
# tc:     .../go-tc-counter      go-tc-counter      name=go-tc-counter      go-tc-counter-maps      tc_stats_map
# app:    .../go-app-counter     go-app-counter     name=go-app-counter     go-app-counter-maps     <six maps>  (multi-map case)

NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
echo "node (kubectl + docker) = $NODE"
```

Note: `zsh` makes `UID` read-only, so this runbook uses `PUID` for the
pod UID. Do not rename it back to `UID`.

## 0. Confirm the deployed driver is this branch

Confirm the deployed build by its version stamp. This works on a fresh
cluster, before any publish has happened -- the line is logged at daemon
startup. The `version="<short-sha>-..."` should match
`git rev-parse --short HEAD`:

```bash
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=2000 \
  | grep 'starting bpfman' | tail -1
```

The CSI plugin logs via Go's slog with `component=csi` from startup too
(e.g. "gRPC server listening", "NodeGetInfo response"), so a non-empty
result here is a second confirmation that does not need a publish:

```bash
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=2000 \
  | grep 'component=csi' | tail -3
```

Uniquely to this branch, each *publish* records `accessMode=`. That line
exists only once a pod has mounted, so use it as a confirmation after
section 1, not on a fresh cluster:

```bash
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=400 \
  | grep -E 'component=csi.*accessMode=' | tail -2
```

Tail the CSI driver live in another terminal while you run the rest:

```bash
kubectl -n "$BPFMAN_NS" logs -f ds/bpfman-daemon -c bpfman | grep --line-buffered component=csi
```

## 1. Load the example

`apply -k` creates the namespace, the `ClusterBpfApplication` (the
bytecode/program the operator loads), and the userspace DaemonSet whose
pod consumes the map through an inline ephemeral CSI volume. The pod
reaching `Running` is itself proof the publish succeeded -- a failed
`NodePublishVolume` leaves the pod stuck in `ContainerCreating`.

```bash
kubectl apply -k "$EXAMPLE_DIR"
kubectl -n "$APP_NS" rollout status ds/"${APP_NS}-ds" --timeout=90s
kubectl -n "$APP_NS" get pods -o wide
```

Resolve the identifiers the next steps need:

```bash
POD=$(kubectl -n "$APP_NS" get pod -l "$APP_LABEL" -o jsonpath='{.items[0].metadata.name}')
PUID=$(kubectl -n "$APP_NS" get pod "$POD" -o jsonpath='{.metadata.uid}')
TARGET="/var/lib/kubelet/pods/$PUID/volumes/kubernetes.io~csi/$VOL_NAME/mount"
VOLID=$(kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=8000 \
  | awk -v u="$PUID" '/NodePublishVolume succeeded/ && $0 ~ u {
      for (i=1;i<=NF;i++) if ($i ~ /^volumeID=/) { sub(/^volumeID=/,"",$i); print $i }
    }' | tail -1)
echo "pod=$POD"; echo "uid=$PUID"; echo "volumeID=$VOLID"; echo "target=$TARGET"
```

## 2. See the parameters kubelet sends

The publish request log line carries the full `volumeContext` plus the
fields this branch validates: `readonly`, `fsGroup`, `accessMode`.

```bash
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=8000 \
  | grep "component=csi" | grep "$PUID" \
  | grep -E 'NodePublishVolume (request|succeeded)' | tail -2
```

Expect, for an ephemeral inline volume: `readonly=false` (the
`readOnly:true` in the example is container-side, not the CSI volume
source), `fsGroup=65534`, `accessMode=SINGLE_NODE_WRITER`, and a
`volumeContext` containing `csi.bpfman.io/program`, `csi.bpfman.io/maps`,
and the kubelet-injected `csi.storage.k8s.io/*` keys.

## 3. Inspect how the mount is built on the node

The driver creates a per-pod bpffs in its own mount namespace, re-pins
the requested map into it, chowns+chmods it to the fsGroup, then
bind-mounts it onto the kubelet target. Only the target bind propagates
into the node namespace (rshared), so that is what you inspect here. The
key checks: exactly one `bpf` mount at the target (no stacking), and the
map pin group-owned by the fsGroup with mode 0660.

```bash
docker exec "$NODE" sh -c '
  T="'"$TARGET"'"
  echo "--- mount at target ---"; findmnt -no TARGET,SOURCE,FSTYPE "$T"
  echo "--- mounts stacked at target (want 1) ---"; grep -c " $T " /proc/self/mountinfo
  echo "--- map pin (want root:65534 mode 0660) ---"; ls -ln "$T"
'
```

## 4. Prove idempotency (a duplicate publish does not stack)

The CSIDriver has `requiresRepublish:false`, so kubelet will not
re-publish on its own. Induce the canonical real-world idempotency event
-- kubelet re-publishing the same volume after a restart -- and confirm
the driver short-circuits instead of stacking a second mount.

```bash
before=$(kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=8000 \
  | grep -c "NodePublishVolume request.*$VOLID")
echo "publish requests for $VOLID before: $before"

docker exec "$NODE" systemctl restart kubelet

# Poll up to ~60s for the re-publish; mounts-at-target must stay 1.
for i in $(seq 1 12); do
  sleep 5
  now=$(kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=8000 \
    | grep -c "NodePublishVolume request.*$VOLID")
  mc=$(docker exec "$NODE" sh -c "grep -c ' $TARGET ' /proc/self/mountinfo")
  echo "t=$((i*5))s  publish-requests=$now  mounts-at-target=$mc"
  [ "$now" -gt "$before" ] && { echo ">>> kubelet re-published"; break; }
done
```

Then confirm the second publish took the idempotent path:

```bash
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=8000 \
  | grep "$VOLID" | grep -iE 'already published|Unpublish' | tail -3
```

Expected: `publish-requests` increases by at least one after the restart
(the absolute count depends on how many operator-race retries preceded
steady state, so it may be `4 -> 5` rather than `1 -> 2` -- only the
increase matters, which is why the loop compares against `$before`),
`mounts-at-target` stays `1`, and the driver logs
`NodePublishVolume already published; returning OK`. On the
pre-idempotency code the second publish would have stacked a fresh empty
bpffs over the first and leaked it.

## 5. Unload and verify there is no residue

`delete -k` drives `NodeUnpublishVolume`, which unmounts the target and
removes the per-pod bpffs. Capture the target before deleting (the pod
UID is gone afterwards).

```bash
T="$TARGET"   # snapshot; $PUID-derived paths vanish with the pod
kubectl delete -k "$EXAMPLE_DIR" --wait=true

sleep 3
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=8000 \
  | grep "$VOLID" | grep -iE 'Unpublish' | tail -2

docker exec "$NODE" sh -c '
  T="'"$T"'"
  echo -n "mounts-at-target (want 0): "; grep -c " $T " /proc/self/mountinfo || true
  echo -n "pod volume dir exists (want no): "; test -e "$T" && echo yes || echo no
  echo -n "per-pod bpffs dirs left (want 0): "; ls /run/bpfman/csi/fs/ 2>/dev/null | grep -c "'"$VOLID"'" || true
'
```

`grep -c` exits non-zero when it counts zero matches; the `|| true`
keeps that from looking like a failure. Zero is the result you want.

## 6. Restore (optional)

```bash
kubectl apply -k "$EXAMPLE_DIR"
kubectl -n "$APP_NS" rollout status ds/"${APP_NS}-ds" --timeout=90s
```

## 7. Failure modes (break tests)

These probe what happens when a pod hands the driver bad input, or when
the userspace pod beats the operator to the program. The point is to
confirm the driver fails *safely*: a precise error to the pod, no
partial mount left behind, and automatic recovery once a transient cause
clears. They need the baseline program loaded (section 1) so the
"wrong map on a real program" case has a real program to reject against.

```bash
kubectl create namespace csi-break
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata: {name: valid, namespace: csi-break}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10, volumeMounts: [{name: m, mountPath: /maps, readOnly: true}]}]
  volumes: [{name: m, csi: {driver: csi.bpfman.io, volumeAttributes: {csi.bpfman.io/program: go-xdp-counter-example, csi.bpfman.io/maps: xdp_stats_map}}}]
---
apiVersion: v1
kind: Pod
metadata: {name: bad-mapname-traversal, namespace: csi-break}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10, volumeMounts: [{name: m, mountPath: /maps, readOnly: true}]}]
  volumes: [{name: m, csi: {driver: csi.bpfman.io, volumeAttributes: {csi.bpfman.io/program: go-xdp-counter-example, csi.bpfman.io/maps: ../escape}}}]
---
apiVersion: v1
kind: Pod
metadata: {name: wrong-map, namespace: csi-break}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10, volumeMounts: [{name: m, mountPath: /maps, readOnly: true}]}]
  volumes: [{name: m, csi: {driver: csi.bpfman.io, volumeAttributes: {csi.bpfman.io/program: go-xdp-counter-example, csi.bpfman.io/maps: nonexistent_map}}}]
---
apiVersion: v1
kind: Pod
metadata: {name: no-program, namespace: csi-break}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10, volumeMounts: [{name: m, mountPath: /maps, readOnly: true}]}]
  volumes: [{name: m, csi: {driver: csi.bpfman.io, volumeAttributes: {csi.bpfman.io/program: does-not-exist, csi.bpfman.io/maps: xdp_stats_map}}}]
---
apiVersion: v1
kind: Pod
metadata: {name: empty-maps, namespace: csi-break}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10, volumeMounts: [{name: m, mountPath: /maps, readOnly: true}]}]
  volumes: [{name: m, csi: {driver: csi.bpfman.io, volumeAttributes: {csi.bpfman.io/program: go-xdp-counter-example, csi.bpfman.io/maps: ""}}}]
EOF
sleep 20
kubectl -n csi-break get pods -o wide
```

Expected: `valid` reaches `Running`; the four others stay
`ContainerCreating`. Each rejection carries a specific gRPC code:

| Pod | attack | gRPC code |
|---|---|---|
| `bad-mapname-traversal` | `maps: ../escape` | `InvalidArgument` -- fails closed, never escapes the maps dir |
| `empty-maps` | `maps: ""` | `InvalidArgument` |
| `no-program` | `program: does-not-exist` | `NotFound` -- logged WARN "program not yet loaded", retried, self-heals |
| `wrong-map` | `maps: nonexistent_map` | `NotFound` -- "map ... is not published by program ..."; rolls back |

What the pod author sees (the gRPC message is verbatim in the event):

```bash
for p in bad-mapname-traversal wrong-map no-program empty-maps; do
  echo "[$p]"
  kubectl -n csi-break get events --field-selector involvedObject.name=$p \
    | grep -iE 'FailedMount|MountVolume' | tail -1
done
```

The driver side shows only what the driver itself logs: the WARN for the
operator race, and the rollback line for `wrong-map` (which fails after
creating the per-pod bpffs). The rejection reasons -- `invalid ...`,
`... at least one ... map ...`, `... is not published by program ...` --
are returned to kubelet as the gRPC status and appear in the pod events
above, not in the driver log:

```bash
kubectl -n "$BPFMAN_NS" logs ds/bpfman-daemon -c bpfman --tail=400 \
  | grep "component=csi" \
  | grep -iE 'program not yet loaded|rolling back' | tail
```

Confirm no failure left residue. Every failed target must show zero
mounts, and no orphan per-pod bpffs may survive -- especially
`wrong-map`, whose bpffs is created then unwound:

```bash
# Per-failed-pod target mount count (all want 0; the valid pod wants 1).
for p in valid bad-mapname-traversal wrong-map no-program empty-maps; do
  u=$(kubectl -n csi-break get pod $p -o jsonpath='{.metadata.uid}' 2>/dev/null)
  [ -z "$u" ] && continue
  T="/var/lib/kubelet/pods/$u/volumes/kubernetes.io~csi/m/mount"
  echo "$p: mounts=$(docker exec "$NODE" sh -c "grep -c ' $T ' /proc/self/mountinfo" 2>/dev/null)"
done

# Orphan per-pod bpffs in the driver namespace: only active mounts (valid +
# the example pod) should appear, none of the failed volumeIDs.
docker exec "$NODE" sh -c '
  for p in $(pgrep -f bpfman); do [ -d "/proc/$p/root/run/bpfman/csi/fs" ] && DPID=$p; done
  ls -1 /proc/${DPID:-1}/root/run/bpfman/csi/fs/ 2>&1'
```

### Self-heal (the operator race)

A pod stuck because its program is not loaded yet must recover on its
own once the program appears -- no pod change. Unload the program,
create a dependent pod, then reload and watch it heal.

```bash
kubectl delete clusterbpfapplication.bpfman.io go-xdp-counter-example --wait=true
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata: {name: selfheal, namespace: csi-break}
spec:
  containers: [{name: c, image: registry.k8s.io/pause:3.10, volumeMounts: [{name: m, mountPath: /maps, readOnly: true}]}]
  volumes: [{name: m, csi: {driver: csi.bpfman.io, volumeAttributes: {csi.bpfman.io/program: go-xdp-counter-example, csi.bpfman.io/maps: xdp_stats_map}}}]
EOF
sleep 12
kubectl -n csi-break get pod selfheal            # ContainerCreating
kubectl apply -k "$EXAMPLE_DIR"                  # operator catches up
for i in $(seq 1 18); do
  sleep 5
  st=$(kubectl -n csi-break get pod selfheal -o jsonpath='{.status.phase}/{.status.containerStatuses[0].ready}')
  echo "t=$((i*5))s selfheal=$st"
  [ "$st" = "Running/true" ] && break
done                                             # flips to Running within a retry cycle
```

### Cleanup

```bash
kubectl delete namespace csi-break --wait=true
```

## Notes and gotchas

- Right container: the CSI plugin is in `-c bpfman`. The
  `node-driver-registrar` sidecar only handles plugin registration; its
  logs will not show publish events.
- The publish is a single ~15ms burst at mount time, not a stream. The
  steady traffic you see is `NodeGetCapabilities` (kubelet polls it) and
  `Get` from the server component. Grep for `NodePublishVolume` rather
  than watching for it to scroll by.
- The per-pod bpffs (`/run/bpfman/csi/fs/<volumeID>`) lives in the
  driver's mount namespace and will not appear in the node's
  `/proc/self/mountinfo`. To see it, look inside the `bpfman` container's
  namespace; the node only ever sees the propagated target bind.
- Multi-map check: use `go-app-counter`, whose volume requests six maps
  in one `csi.bpfman.io/maps` value; step 3's `ls -ln "$T"` then lists
  all six pins.
