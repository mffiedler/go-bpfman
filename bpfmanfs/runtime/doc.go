// Package runtime provides I/O operations for bpfman's runtime environment.
//
// This package handles the one-time setup required before constructing a
// manager: creating runtime directories and mounting bpffs. It is separate
// from the bpfmanfs package to maintain the distinction between pure path
// computation (bpfmanfs) and actual I/O operations (bpfmanfs/runtime).
//
// The New function returns a FilesystemContext capability token that proves
// the filesystem is ready. This token is required by manager.New(), enforcing
// that setup is complete before any operations.
//
// Usage:
//
//	layout, err := bpfmanfs.New("/run/bpfman")
//	if err != nil {
//	    return err
//	}
//	fsctx, err := runtime.New(layout, runtime.RealMounter{}, logger)
//	if err != nil {
//	    return err
//	}
//	mgr, err := manager.New(fsctx, store, kernel, discoverer, logger)
//
// For tests, use NoOpMounter to skip actual bpffs mounting:
//
//	fsctx, _ := runtime.New(layout, runtime.NoOpMounter{}, logger)
package runtime
