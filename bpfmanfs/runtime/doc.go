// Package runtime provides I/O operations for bpfman's runtime environment.
//
// This package handles the one-time setup required before constructing a
// manager: creating runtime directories and mounting bpffs. It is separate
// from the bpfmanfs package to maintain the distinction between pure path
// computation (bpfmanfs) and actual I/O operations (bpfmanfs/runtime).
//
// Usage:
//
//	root, err := bpfmanfs.New("/run")
//	if err != nil {
//	    return err
//	}
//	if err := runtime.Ensure(root, runtime.RealMounter{}, logger); err != nil {
//	    return err
//	}
//	mgr, err := manager.New(root, store, kernel, discoverer, logger)
//
// For tests, use NoOpMounter to skip actual bpffs mounting:
//
//	runtime.Ensure(root, runtime.NoOpMounter{}, logger)
package runtime
