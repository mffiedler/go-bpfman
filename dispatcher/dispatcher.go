package dispatcher

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/cilium/ebpf"
)

// MaxPrograms is the maximum number of programs that can be chained.
const MaxPrograms = 10

// XDP dispatcher bytecode - compiled from xdp_dispatcher_v2.bpf.c
//
//go:embed xdp_dispatcher_v2.bpf.o
var xdpDispatcherBytes []byte

// TC dispatcher bytecode - compiled from tc_dispatcher.bpf.c
//
//go:embed tc_dispatcher.bpf.o
var tcDispatcherBytes []byte

// XDP dispatcher v3 bytecode - compiled from xdp_dispatcher_v3.bpf.c
// Uses double-buffered BPF maps for runtime dispatch configuration.
//
//go:embed xdp_dispatcher_v3.bpf.o
var xdpDispatcherV3Bytes []byte

// TC dispatcher v2 bytecode - compiled from tc_dispatcher_v2.bpf.c
// Uses double-buffered BPF maps for runtime dispatch configuration.
//
//go:embed tc_dispatcher_v2.bpf.o
var tcDispatcherV2Bytes []byte

// XDPConfig configures the XDP dispatcher.
// This must match struct xdp_dispatcher_conf in xdp_dispatcher_v2.bpf.c.
type XDPConfig struct {
	Magic             uint8
	DispatcherVersion uint8
	NumProgsEnabled   uint8
	IsXDPFrags        uint8
	ChainCallActions  [MaxPrograms]uint32
	RunPrios          [MaxPrograms]uint32
	ProgramFlags      [MaxPrograms]uint32
}

// TCConfig configures the TC dispatcher.
// This must match struct tc_dispatcher_config in tc_dispatcher.bpf.c.
type TCConfig struct {
	NumProgsEnabled  uint8
	_                [3]uint8 // padding for alignment
	ChainCallActions [MaxPrograms]uint32
	RunPrios         [MaxPrograms]uint32
}

const (
	// XDP dispatcher constants from xdp_dispatcher_v2.bpf.c
	xdpDispatcherMagic   = 236
	xdpDispatcherVersion = 2

	// DefaultPriority is the default run priority for dispatcher slots.
	DefaultPriority = 50
)

// XDPAction represents XDP return codes for proceed-on configuration.
type XDPAction uint32

const (
	XDPAborted  XDPAction = 0
	XDPDrop     XDPAction = 1
	XDPPass     XDPAction = 2
	XDPTX       XDPAction = 3
	XDPRedirect XDPAction = 4
)

// ProceedOnMask returns a bitmask for the given XDP actions.
// If a program returns one of these actions, the dispatcher continues
// to the next program in the chain.
func ProceedOnMask(actions ...XDPAction) uint32 {
	var mask uint32
	for _, a := range actions {
		mask |= 1 << uint32(a)
	}
	return mask
}

// NewXDPConfig creates a default XDP dispatcher config.
func NewXDPConfig(numProgs int) XDPConfig {
	cfg := XDPConfig{
		Magic:             xdpDispatcherMagic,
		DispatcherVersion: xdpDispatcherVersion,
		NumProgsEnabled:   uint8(numProgs),
	}
	for i := 0; i < MaxPrograms; i++ {
		cfg.RunPrios[i] = DefaultPriority
	}
	return cfg
}

// NewTCConfig creates a default TC dispatcher config.
func NewTCConfig(numProgs int) TCConfig {
	cfg := TCConfig{
		NumProgsEnabled: uint8(numProgs),
	}
	for i := 0; i < MaxPrograms; i++ {
		cfg.RunPrios[i] = DefaultPriority
	}
	return cfg
}

// LoadXDPDispatcher loads the XDP dispatcher with the given config.
func LoadXDPDispatcher(cfg XDPConfig) (*ebpf.CollectionSpec, error) {
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(xdpDispatcherBytes))
	if err != nil {
		return nil, fmt.Errorf("load XDP dispatcher spec: %w", err)
	}

	confVar, ok := spec.Variables["conf"]
	if !ok {
		return nil, fmt.Errorf("XDP dispatcher missing 'conf' variable")
	}
	if err := confVar.Set(cfg); err != nil {
		return nil, fmt.Errorf("set XDP dispatcher config: %w", err)
	}

	return spec, nil
}

// LoadTCDispatcher loads the TC dispatcher with the given config.
func LoadTCDispatcher(cfg TCConfig) (*ebpf.CollectionSpec, error) {
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(tcDispatcherBytes))
	if err != nil {
		return nil, fmt.Errorf("load TC dispatcher spec: %w", err)
	}

	confVar, ok := spec.Variables["CONFIG"]
	if !ok {
		return nil, fmt.Errorf("TC dispatcher missing 'CONFIG' variable")
	}
	if err := confVar.Set(cfg); err != nil {
		return nil, fmt.Errorf("set TC dispatcher config: %w", err)
	}

	return spec, nil
}

// RuntimeConfig is the Go representation of the BPF
// dispatcher_runtime struct used by the v3/v2 map-based dispatchers.
// It is written to the dispatcher_config BPF map to control the
// execution order at runtime.
type RuntimeConfig struct {
	NumProgsEnabled  uint32
	RunOrder         [MaxPrograms]uint32
	ChainCallActions [MaxPrograms]uint32
}

// ConfigMapName is the BPF map name for the double-buffer config.
const ConfigMapName = "dispatcher_config"

// ActiveMapName is the BPF map name for the active buffer index.
const ActiveMapName = "active_config"

// LoadXDPDispatcherV3 loads the map-based XDP dispatcher spec.
// The .rodata config is still injected for XDP metadata compatibility
// (magic, version, is_xdp_frags, program_flags, run_prios). The
// dispatch loop reads from BPF maps instead of .rodata.
func LoadXDPDispatcherV3(cfg XDPConfig) (*ebpf.CollectionSpec, error) {
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(xdpDispatcherV3Bytes))
	if err != nil {
		return nil, fmt.Errorf("load XDP v3 dispatcher spec: %w", err)
	}

	confVar, ok := spec.Variables["conf"]
	if !ok {
		return nil, fmt.Errorf("XDP v3 dispatcher missing 'conf' variable")
	}
	if err := confVar.Set(cfg); err != nil {
		return nil, fmt.Errorf("set XDP v3 dispatcher config: %w", err)
	}

	return spec, nil
}

// LoadTCDispatcherV2 loads the map-based TC dispatcher spec.
// No .rodata injection is needed -- TC v2 has no metadata to
// preserve. The dispatch loop is entirely map-driven.
func LoadTCDispatcherV2() (*ebpf.CollectionSpec, error) {
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(tcDispatcherV2Bytes))
	if err != nil {
		return nil, fmt.Errorf("load TC v2 dispatcher spec: %w", err)
	}
	return spec, nil
}

// SlotName returns the function name for a dispatcher slot (0-9).
// This is the target function name used for BPF extension attachment.
func SlotName(position int) string {
	return fmt.Sprintf("prog%d", position)
}
