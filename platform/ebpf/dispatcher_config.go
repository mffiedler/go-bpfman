package ebpf

import (
	"context"
	"fmt"

	"github.com/cilium/ebpf"

	"github.com/frobware/go-bpfman/dispatcher"
)

// UpdateDispatcherConfig atomically updates the dispatcher runtime
// configuration. It loads the pinned config and active maps, writes
// the new config to the inactive buffer, then flips the active index.
func (k *kernelAdapter) UpdateDispatcherConfig(ctx context.Context, configMapPin, activeMapPin string, config dispatcher.RuntimeConfig) error {
	configMap, err := ebpf.LoadPinnedMap(configMapPin, nil)
	if err != nil {
		return fmt.Errorf("load pinned config map %s: %w", configMapPin, err)
	}
	defer configMap.Close()

	activeMap, err := ebpf.LoadPinnedMap(activeMapPin, nil)
	if err != nil {
		return fmt.Errorf("load pinned active map %s: %w", activeMapPin, err)
	}
	defer activeMap.Close()

	// Read current active index
	var active uint32
	if err := activeMap.Lookup(uint32(0), &active); err != nil {
		return fmt.Errorf("read active index: %w", err)
	}

	// Write to the inactive buffer
	inactive := 1 - active
	if err := configMap.Put(inactive, &config); err != nil {
		return fmt.Errorf("write config to buffer %d: %w", inactive, err)
	}

	// Atomically flip the active index
	if err := activeMap.Put(uint32(0), inactive); err != nil {
		return fmt.Errorf("flip active index to %d: %w", inactive, err)
	}

	k.logger.Debug("updated dispatcher config",
		"config_map", configMapPin,
		"active_map", activeMapPin,
		"new_active", inactive,
		"num_progs", config.NumProgsEnabled)

	return nil
}
