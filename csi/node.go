package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

// mapsMode is the permission mode for CSI-exposed maps (owner+group read/write).
const mapsMode = 0o0660

// CSI volume attribute keys matching upstream Rust bpfman.
const (
	// VolumeAttrProgram specifies the program name to look up.
	// This is matched against the bpfman.io/ProgramName metadata.
	VolumeAttrProgram = "csi.bpfman.io/program"

	// VolumeAttrMaps specifies a comma-separated list of map names to expose.
	VolumeAttrMaps = "csi.bpfman.io/maps"

	// MetadataKeyProgramName is the metadata key used to identify programs.
	MetadataKeyProgramName = "bpfman.io/ProgramName"
)

// NodeGetInfo returns information about this node.
func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	d.logger.Debug("NodeGetInfo",
		"method", "Node.NodeGetInfo",
	)

	resp := &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
	}

	d.logger.Info("NodeGetInfo response",
		"method", "Node.NodeGetInfo",
		"nodeID", resp.NodeId,
	)

	return resp, nil
}

// NodeGetCapabilities returns the capabilities of this node plugin.
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	d.logger.Debug("NodeGetCapabilities",
		"method", "Node.NodeGetCapabilities",
	)

	resp := &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP,
					},
				},
			},
		},
	}

	d.logger.Info("NodeGetCapabilities response",
		"method", "Node.NodeGetCapabilities",
		"capabilities", len(resp.Capabilities),
	)

	return resp, nil
}

// NodePublishVolume mounts BPF maps to the target path.
//
// The driver looks up programs by csi.bpfman.io/program metadata,
// re-pins requested maps to a per-pod bpffs, and bind-mounts
// that bpffs to the container.
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	volumeContext := req.GetVolumeContext()
	readonly := req.GetReadonly()

	// Extract fsGroup from volume capability if present.
	// This allows unprivileged containers to access the maps.
	var fsGroup int = -1
	if volCap := req.GetVolumeCapability(); volCap != nil {
		if mount := volCap.GetMount(); mount != nil {
			if groupStr := mount.GetVolumeMountGroup(); groupStr != "" {
				if gid, err := strconv.Atoi(groupStr); err == nil {
					fsGroup = gid
				}
			}
		}
	}

	d.logger.Info("NodePublishVolume request",
		"method", "Node.NodePublishVolume",
		"volumeID", volumeID,
		"targetPath", targetPath,
		"volumeContext", volumeContext,
		"readonly", readonly,
		"fsGroup", fsGroup,
	)

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	programName := volumeContext[VolumeAttrProgram]
	mapsStr := volumeContext[VolumeAttrMaps]

	if programName == "" || mapsStr == "" {
		return nil, status.Error(codes.InvalidArgument,
			"csi.bpfman.io/program and csi.bpfman.io/maps are required")
	}

	if d.programFinder == nil || d.kernel == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"bpfman integration not configured; programFinder and kernel required")
	}

	// 1. Find program by metadata (reconciled with kernel state)
	metadata, _, err := d.programFinder.FindLoadedProgramByMetadata(ctx, MetadataKeyProgramName, programName)
	if err != nil {
		// Return appropriate gRPC code based on error type.
		// NotFound is expected during reconciliation — the CSI
		// driver may ask before the operator has loaded the program.
		switch {
		case errors.Is(err, platform.ErrRecordNotFound):
			d.logger.Warn("program not yet loaded",
				"programName", programName,
				"error", err,
			)
			return nil, status.Errorf(codes.NotFound, "program %q not found", programName)
		case errors.Is(err, manager.ErrMultipleProgramsFound), errors.Is(err, manager.ErrMultipleMapOwners):
			d.logger.Error("failed to find program",
				"programName", programName,
				"error", err,
			)
			return nil, status.Errorf(codes.FailedPrecondition, "program %q: %v", programName, err)
		default:
			d.logger.Error("failed to find program",
				"programName", programName,
				"error", err,
			)
			return nil, status.Errorf(codes.Internal, "failed to find program %q: %v", programName, err)
		}
	}

	// 2. Get the maps directory from the program (may differ from PinPath if sharing maps)
	mapPinPath := metadata.Handles.MapPinPath
	if mapPinPath == "" {
		return nil, status.Errorf(codes.Internal, "program %q has no map pin path", programName)
	}

	d.logger.Info("found program",
		"programName", programName,
		"mapPinPath", mapPinPath,
	)

	// 3. Create per-pod bpffs directory
	podBpffs := filepath.Join(d.csiFsRoot, volumeID)
	if err := os.MkdirAll(podBpffs, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create bpffs dir %q: %v", podBpffs, err)
	}

	// 4. Mount bpffs on the per-pod directory
	if err := mountBpffs(podBpffs); err != nil {
		if rmErr := os.RemoveAll(podBpffs); rmErr != nil {
			d.logger.Warn("failed to remove pod bpffs directory during cleanup", "path", podBpffs, "error", rmErr)
		}
		return nil, status.Errorf(codes.Internal, "failed to mount bpffs at %q: %v", podBpffs, err)
	}

	// 5. Set group ownership on the bpffs directory if fsGroup is specified
	if fsGroup >= 0 {
		if err := unix.Chown(podBpffs, -1, fsGroup); err != nil {
			d.logger.Warn("failed to chown bpffs directory",
				"path", podBpffs,
				"gid", fsGroup,
				"error", err,
			)
		}
	}

	// 6. Re-pin each requested map
	mapNames := strings.Split(mapsStr, ",")
	for _, mapName := range mapNames {
		mapName = strings.TrimSpace(mapName)
		if mapName == "" {
			continue
		}

		srcPath := filepath.Join(mapPinPath, mapName)
		dstPath := filepath.Join(podBpffs, mapName)

		d.logger.Debug("re-pinning map",
			"map", mapName,
			"src", srcPath,
			"dst", dstPath,
		)

		if err := d.kernel.RepinMap(ctx, srcPath, dstPath); err != nil {
			// Cleanup on failure
			unix.Unmount(podBpffs, 0)
			if rmErr := os.RemoveAll(podBpffs); rmErr != nil {
				d.logger.Warn("failed to remove pod bpffs directory during cleanup", "path", podBpffs, "error", rmErr)
			}
			return nil, status.Errorf(codes.Internal, "failed to re-pin map %q: %v", mapName, err)
		}

		// Set group ownership and permissions on the map if fsGroup is specified.
		// This allows unprivileged containers to access the maps.
		if fsGroup >= 0 {
			if err := unix.Chown(dstPath, -1, fsGroup); err != nil {
				d.logger.Warn("failed to chown map",
					"path", dstPath,
					"gid", fsGroup,
					"error", err,
				)
			}
			if err := os.Chmod(dstPath, mapsMode); err != nil {
				d.logger.Warn("failed to chmod map",
					"path", dstPath,
					"mode", mapsMode,
					"error", err,
				)
			}
		}
	}

	// 7. Create target directory and bind-mount
	if err := os.MkdirAll(targetPath, 0755); err != nil {
		unix.Unmount(podBpffs, 0)
		if rmErr := os.RemoveAll(podBpffs); rmErr != nil {
			d.logger.Warn("failed to remove pod bpffs directory during cleanup", "path", podBpffs, "error", rmErr)
		}
		return nil, status.Errorf(codes.Internal, "failed to create target path: %v", err)
	}

	flags := uintptr(unix.MS_BIND)
	if readonly {
		flags |= unix.MS_RDONLY
	}

	if err := unix.Mount(podBpffs, targetPath, "", flags, ""); err != nil {
		unix.Unmount(podBpffs, 0)
		if rmErr := os.RemoveAll(podBpffs); rmErr != nil {
			d.logger.Warn("failed to remove pod bpffs directory during cleanup", "path", podBpffs, "error", rmErr)
		}
		return nil, status.Errorf(codes.Internal, "failed to bind-mount %q to %q: %v", podBpffs, targetPath, err)
	}

	d.logger.Info("NodePublishVolume succeeded",
		"method", "Node.NodePublishVolume",
		"volumeID", volumeID,
		"programName", programName,
		"maps", mapsStr,
		"podBpffs", podBpffs,
		"targetPath", targetPath,
		"readonly", readonly,
		"fsGroup", fsGroup,
	)

	return &csi.NodePublishVolumeResponse{}, nil
}

// bpffsMagic is the magic number for bpffs (from statfs).
const bpffsMagic = 0xcafe4a11

// mountBpffs mounts a bpffs filesystem at the given path.
func mountBpffs(path string) error {
	if err := unix.Mount("bpf", path, "bpf", 0, ""); err != nil {
		return err
	}

	// Verify the mount is actually bpffs - catches misconfiguration early
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		unix.Unmount(path, 0)
		return err
	}
	if stat.Type != bpffsMagic {
		unix.Unmount(path, 0)
		return unix.EINVAL
	}

	return nil
}

// NodeUnpublishVolume unmounts the volume from the target path.
// It also cleans up the per-pod bpffs.
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	d.logger.Info("NodeUnpublishVolume request",
		"method", "Node.NodeUnpublishVolume",
		"volumeID", volumeID,
		"targetPath", targetPath,
	)

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	// 1. Unmount the bind-mount from the container
	if err := unix.Unmount(targetPath, 0); err != nil {
		// Ignore "not mounted" errors for idempotency
		if err != unix.EINVAL && err != unix.ENOENT {
			return nil, status.Errorf(codes.Internal, "failed to unmount %q: %v", targetPath, err)
		}
	}

	// 2. Remove the target directory
	if err := os.RemoveAll(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove target path: %v", err)
	}

	// 3. Clean up per-pod bpffs
	podBpffs := filepath.Join(d.csiFsRoot, volumeID)
	if _, err := os.Stat(podBpffs); err == nil {
		// Unmount the per-pod bpffs
		if err := unix.Unmount(podBpffs, 0); err != nil {
			// Ignore "not mounted" errors
			if err != unix.EINVAL && err != unix.ENOENT {
				d.logger.Warn("failed to unmount per-pod bpffs",
					"path", podBpffs,
					"error", err,
				)
			}
		}

		// Remove the directory
		if err := os.RemoveAll(podBpffs); err != nil {
			d.logger.Warn("failed to remove per-pod bpffs directory",
				"path", podBpffs,
				"error", err,
			)
		}
	}

	d.logger.Info("NodeUnpublishVolume succeeded",
		"method", "Node.NodeUnpublishVolume",
		"volumeID", volumeID,
		"targetPath", targetPath,
	)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeStageVolume is called before NodePublishVolume if staging is advertised.
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	d.logger.Warn("NodeStageVolume called but not implemented",
		"method", "Node.NodeStageVolume",
		"volumeID", req.GetVolumeId(),
	)
	return nil, status.Error(codes.Unimplemented, "NodeStageVolume not supported")
}

// NodeUnstageVolume is the counterpart to NodeStageVolume.
func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	d.logger.Warn("NodeUnstageVolume called but not implemented",
		"method", "Node.NodeUnstageVolume",
		"volumeID", req.GetVolumeId(),
	)
	return nil, status.Error(codes.Unimplemented, "NodeUnstageVolume not supported")
}

// NodeGetVolumeStats returns statistics about a volume.
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	d.logger.Warn("NodeGetVolumeStats called but not implemented",
		"method", "Node.NodeGetVolumeStats",
		"volumeID", req.GetVolumeId(),
	)
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats not supported")
}

// NodeExpandVolume expands a volume on the node.
func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	d.logger.Warn("NodeExpandVolume called but not implemented",
		"method", "Node.NodeExpandVolume",
		"volumeID", req.GetVolumeId(),
	)
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume not supported")
}
