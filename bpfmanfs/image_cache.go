package bpfmanfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// BytecodeFile is the name of the extracted bytecode file within a cache entry.
	BytecodeFile = "bytecode.o"

	// MetadataFile is the name of the cached metadata file within a cache entry.
	MetadataFile = "metadata.json"
)

// ImageCache provides validated operations on the OCI image cache.
// Obtain via NewImageCache() with a validated root path.
//
// ImageCache is separate from FSLayout because they have different
// lifecycles:
//   - FSLayout: /run/bpfman (tmpfs, cleared on reboot)
//   - ImageCache: /var/cache/bpfman (persistent, survives reboots)
type ImageCache struct {
	root string
}

// NewImageCache creates an ImageCache for the given root directory.
// The root path is used directly - callers must provide the full path.
//
// Rejects empty or relative paths to prevent accidental operations
// on system directories.
func NewImageCache(root string) (ImageCache, error) {
	if root == "" {
		return ImageCache{}, fmt.Errorf("bpfmanfs: image cache root cannot be empty")
	}
	if !filepath.IsAbs(root) {
		return ImageCache{}, fmt.Errorf("bpfmanfs: image cache root must be absolute, got %q", root)
	}
	return ImageCache{root: filepath.Clean(root)}, nil
}

// Valid reports whether the ImageCache was obtained from NewImageCache.
func (c ImageCache) Valid() bool {
	return c.root != ""
}

// mustValid panics if c was not obtained from NewImageCache.
func (c ImageCache) mustValid() {
	if !c.Valid() {
		panic("bpfmanfs: zero ImageCache used; obtain via NewImageCache()")
	}
}

// Root returns the cache root path.
func (c ImageCache) Root() string {
	c.mustValid()
	return c.root
}

// CacheKey computes a deterministic cache key from a URL.
// The key is a truncated SHA256 hash prefixed with "sha256_".
func (c ImageCache) CacheKey(url string) string {
	c.mustValid()
	hash := sha256.Sum256([]byte(url))
	return "sha256_" + hex.EncodeToString(hash[:16])
}

// CacheKeyDir returns the directory path for a cache key.
// Format: <root>/<cacheKey>
func (c ImageCache) CacheKeyDir(cacheKey string) string {
	c.mustValid()
	return filepath.Join(c.root, cacheKey)
}

// BytecodePath returns the bytecode file path for a cache key.
// Format: <root>/<cacheKey>/bytecode.o
func (c ImageCache) BytecodePath(cacheKey string) string {
	c.mustValid()
	return filepath.Join(c.root, cacheKey, BytecodeFile)
}

// MetadataPath returns the metadata file path for a cache key.
// Format: <root>/<cacheKey>/metadata.json
func (c ImageCache) MetadataPath(cacheKey string) string {
	c.mustValid()
	return filepath.Join(c.root, cacheKey, MetadataFile)
}

// EnsureRoot creates the cache root directory if it does not exist.
func (c ImageCache) EnsureRoot() error {
	c.mustValid()
	if err := os.MkdirAll(c.root, dirMode); err != nil {
		return &PathError{Op: "ensure_root", Path: c.root, Err: err}
	}
	return nil
}

// EnsureCacheDir creates the cache entry directory for a cache key.
func (c ImageCache) EnsureCacheDir(cacheKey string) error {
	c.mustValid()
	dir := c.CacheKeyDir(cacheKey)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return &PathError{Op: "ensure_cache_dir", Path: dir, Err: err}
	}
	return nil
}

// RemoveCacheEntry removes a cache entry directory safely.
// The cacheKey must be a direct child of the cache root.
// Returns nil if the directory does not exist.
//
// Uses safeRemoveAll to verify the target is under the cache root,
// preventing accidental deletion of paths outside the cache.
func (c ImageCache) RemoveCacheEntry(cacheKey string) error {
	c.mustValid()
	target := c.CacheKeyDir(cacheKey)
	return safeRemoveAll(c.root, target)
}

// CreateTempDir creates a temporary directory under the cache root.
// Returns the path and a cleanup function. The cleanup function is safe
// to call even after the directory has been removed or renamed.
//
// The cleanup function uses safeRemoveAll to ensure it only removes
// directories under the cache root.
func (c ImageCache) CreateTempDir() (path string, cleanup func(), err error) {
	c.mustValid()
	if err := c.EnsureRoot(); err != nil {
		return "", nil, err
	}

	tmpDir, err := os.MkdirTemp(c.root, "pull-*")
	if err != nil {
		return "", nil, &PathError{Op: "create_temp_dir", Path: c.root, Err: err}
	}

	cleanup = func() {
		_ = safeRemoveAll(c.root, tmpDir)
	}
	return tmpDir, cleanup, nil
}

// WriteTempFile writes data to a file within a directory.
// The directory should typically be one returned by CreateTempDir.
func (c ImageCache) WriteTempFile(dir, name string, data []byte) error {
	c.mustValid()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, fileMode); err != nil {
		return &PathError{Op: "write_temp_file", Path: path, Err: err}
	}
	return nil
}

// CacheBytecode moves a bytecode file from srcPath to the cache entry.
// It first attempts an atomic rename, falling back to copy if rename fails
// (e.g., cross-device move).
func (c ImageCache) CacheBytecode(cacheKey, srcPath string) error {
	c.mustValid()
	destPath := c.BytecodePath(cacheKey)

	// Try atomic rename first
	if err := os.Rename(srcPath, destPath); err == nil {
		return nil
	}

	// Fall back to copy
	if err := (copyFileOp{src: srcPath, dst: destPath, perm: fileMode}).exec(); err != nil {
		return &PathError{Op: "cache_bytecode", Path: destPath, Err: err}
	}
	return nil
}

// SaveMetadata writes metadata to the cache entry as JSON.
func (c ImageCache) SaveMetadata(cacheKey string, meta any) error {
	c.mustValid()
	path := c.MetadataPath(cacheKey)
	if err := (writeJSONOp{path: path, perm: fileMode, v: meta}).exec(); err != nil {
		return &PathError{Op: "save_metadata", Path: path, Err: err}
	}
	return nil
}

// LoadMetadata reads metadata from the cache entry.
// dest should be a pointer to the type to unmarshal into.
func (c ImageCache) LoadMetadata(cacheKey string, dest any) error {
	c.mustValid()
	path := c.MetadataPath(cacheKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return &PathError{Op: "load_metadata", Path: path, Err: err}
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return &PathError{Op: "load_metadata", Path: path, Err: err}
	}
	return nil
}

// BytecodeExists reports whether the bytecode file exists for a cache key.
func (c ImageCache) BytecodeExists(cacheKey string) bool {
	c.mustValid()
	path := c.BytecodePath(cacheKey)
	_, err := os.Stat(path)
	return err == nil
}
