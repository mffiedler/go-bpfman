#!/usr/bin/env bash
# Baseline full-lifecycle proof for each program type. Sourced after
# harness.sh (which provides go_begin/go_finish/rust_begin/rust_finish,
# veth_up/veth_down, gobpf/rsbpf, goj/rsf, cap).
#
# Lifecycle per type: load -> get program -> attach -> get link ->
# link list -> program list -> detach -> unload -> get (not found).

LIBC_TARGET() { ldd "$(command -v cat)" | awk '/libc\.so/{print $3; exit}'; }

# baseline TYPE OBJ SPEC : runs the lifecycle on both impls. The attach
# verb/args vary by TYPE; everything else is the shared begin/finish.
baseline() {
  local type=$1 obj=$2 spec=$3 tag="$1-baseline" IF=bpfmanpar0 LIBC
  LIBC=$(LIBC_TARGET)
  local needs_iface=0; case "$type" in xdp|tc|tcx) needs_iface=1;; esac
  # attach verb is the same as TYPE except the ret-probes attach via the
  # base probe verb.
  local averb="$type"; case "$type" in kretprobe) averb=kprobe;; uretprobe) averb=uprobe;; esac

  # ---- GO ----
  [ $needs_iface = 1 ] && veth_up "$IF"
  go_begin "$tag" "$obj" "$spec"
  case "$type" in
    xdp)            cap "$GT" gobpf link attach xdp "$GO_PID" "$IF" --priority 100 --metadata owner=parity -o json ;;
    tc|tcx)         cap "$GT" gobpf link attach "$type" "$GO_PID" "$IF" ingress --priority 100 --metadata owner=parity -o json ;;
    tracepoint)     cap "$GT" gobpf link attach tracepoint "$GO_PID" syscalls/sys_enter_kill --metadata owner=parity -o json ;;
    kprobe|kretprobe) cap "$GT" gobpf link attach kprobe "$GO_PID" vfs_read --metadata owner=parity -o json ;;
    uprobe|uretprobe) cap "$GT" gobpf link attach uprobe "$GO_PID" "$LIBC" --fn-name malloc --metadata owner=parity -o json ;;
    fentry|fexit)   cap "$GT" gobpf link attach "$type" "$GO_PID" --metadata owner=parity -o json ;;
  esac
  GO_LINK=$(goj "$CAP_OUT" '.record.id'); go_finish
  [ $needs_iface = 1 ] && veth_down "$IF"

  # ---- RUST ----
  [ $needs_iface = 1 ] && veth_up "$IF"
  rust_begin "$tag" "$obj" "$spec"
  case "$type" in
    xdp)            cap "$RT" rsbpf attach "$R_PID" xdp --iface "$IF" --priority 100 --metadata owner=parity ;;
    tc|tcx)         cap "$RT" rsbpf attach "$R_PID" "$type" --direction ingress --iface "$IF" --priority 100 --metadata owner=parity ;;
    tracepoint)     cap "$RT" rsbpf attach "$R_PID" tracepoint --tracepoint syscalls/sys_enter_kill --metadata owner=parity ;;
    kprobe|kretprobe) cap "$RT" rsbpf attach "$R_PID" kprobe --fn-name vfs_read --metadata owner=parity ;;
    uprobe|uretprobe) cap "$RT" rsbpf attach "$R_PID" uprobe --target "$LIBC" --fn-name malloc --metadata owner=parity ;;
    fentry|fexit)   cap "$RT" rsbpf attach "$R_PID" "$type" --metadata owner=parity ;;
  esac
  R_LINK=$(rsf "$CAP_OUT" 'Link ID'); rust_finish
  [ $needs_iface = 1 ] && veth_down "$IF"
}

baselines_all() {
  baseline xdp        e2e/testdata/bpf/xdp_pass.bpf.o          xdp:pass
  baseline tc         e2e/testdata/bpf/tc_counter.bpf.o        tc:stats
  baseline tcx        e2e/testdata/bpf/tcx_counter.bpf.o       tcx:tcx_stats
  baseline tracepoint e2e/testdata/bpf/tracepoint_counter.bpf.o tracepoint:tracepoint_kill_recorder
  baseline kprobe     e2e/testdata/bpf/kprobe_counter.bpf.o    kprobe:kprobe_counter
  baseline kretprobe  e2e/testdata/bpf/kprobe_counter.bpf.o    kretprobe:kprobe_counter
  baseline uprobe     e2e/testdata/bpf/uprobe_counter.bpf.o    uprobe:uprobe_counter
  baseline uretprobe  e2e/testdata/bpf/uprobe_counter.bpf.o    uretprobe:uprobe_counter
  baseline fentry     e2e/testdata/bpf/fentry_counter.bpf.o    fentry:test_fentry:do_unlinkat
  baseline fexit      e2e/testdata/bpf/fentry_counter.bpf.o    fexit:test_fexit:do_unlinkat
}
