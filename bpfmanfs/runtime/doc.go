// Package runtime provides I/O operations for bpfman's runtime environment.
//
// This package handles the one-time setup required before constructing a
// manager: creating runtime directories and mounting bpffs. It is separate
// from the bpfmanfs package to maintain the distinction between pure path
// computation (bpfmanfs) and actual I/O operations (bpfmanfs/runtime).
//
// Usage:
//
//	layout, err := bpfmanfs.New("/run/bpfman")
//	if err != nil {
//	    return err
//	}
//	if err := runtime.Ensure(layout, runtime.RealMounter{}, logger); err != nil {
//	    return err
//	}
//	mgr, err := manager.New(layout, store, kernel, discoverer, logger)
//
// For tests, use NoOpMounter to skip actual bpffs mounting:
//
//	runtime.Ensure(layout, runtime.NoOpMounter{}, logger)
package runtime
