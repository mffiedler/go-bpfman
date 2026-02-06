package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/manager"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// Load implements the Load RPC method.
func (s *Server) Load(ctx context.Context, req *pb.LoadRequest) (*pb.LoadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.mgr.GCIfNeeded(ctx, true); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}
	defer s.mgr.MarkMutated()

	if req.Bytecode == nil {
		return nil, status.Error(codes.InvalidArgument, "bytecode location is required")
	}

	// Get the bytecode path and optional image source
	var objectPath string
	var imageSource *bpfman.ImageSource
	switch loc := req.Bytecode.Location.(type) {
	case *pb.BytecodeLocation_File:
		objectPath = loc.File
	case *pb.BytecodeLocation_Image:
		if s.puller == nil {
			return nil, status.Error(codes.Unimplemented, "OCI image loading not configured on this server")
		}

		// Convert proto to interpreter types
		pullPolicy := protoToPullPolicy(loc.Image.ImagePullPolicy)
		ref := interpreter.ImageRef{
			URL:        loc.Image.Url,
			PullPolicy: pullPolicy,
		}
		if loc.Image.Username != nil && *loc.Image.Username != "" {
			ref.Auth = &interpreter.ImageAuth{
				Username: *loc.Image.Username,
			}
			if loc.Image.Password != nil {
				ref.Auth.Password = *loc.Image.Password
			}
		}

		// Pull the image
		pulled, err := s.puller.Pull(ctx, ref)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to pull image %s: %v", loc.Image.Url, err)
		}
		objectPath = pulled.ObjectPath
		imageSource = &bpfman.ImageSource{
			URL:        loc.Image.Url,
			Digest:     pulled.Digest,
			PullPolicy: pullPolicy,
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid bytecode location")
	}

	if len(req.Info) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one program info is required")
	}

	resp := &pb.LoadResponse{
		Programs: make([]*pb.LoadResponseInfo, 0, len(req.Info)),
	}

	// Track successfully loaded programs for rollback on failure
	var loadedKernelIDs []uint32

	// Track the first program's kernel ID for map sharing within this request.
	// When loading multiple programs from the same image, subsequent programs
	// share maps with the first program (the "map owner").
	var mapOwnerKernelID uint32

	// rollback unloads all previously loaded programs in reverse order
	rollback := func() {
		for i := len(loadedKernelIDs) - 1; i >= 0; i-- {
			if err := s.mgr.Unload(ctx, loadedKernelIDs[i]); err != nil {
				s.logger.ErrorContext(ctx, "rollback failed", "kernel_id", loadedKernelIDs[i], "error", err)
			}
		}
	}

	// Load each requested program using the manager (transactional)
	// Pin paths are computed from kernel ID, following upstream convention
	for i, info := range req.Info {
		// Validate program name is not empty
		if info.Name == "" {
			rollback()
			return nil, status.Error(codes.InvalidArgument, "program name is required")
		}

		progType, err := protoToBpfmanType(info.ProgramType)
		if err != nil {
			rollback()
			return nil, status.Errorf(codes.InvalidArgument, "invalid program type for %s: %v", info.Name, err)
		}

		// Check for actual type metadata to handle kretprobe/uretprobe
		// which map to KPROBE/UPROBE in the proto enum.
		progType = resolveActualType(progType, info.Name, req.Metadata)

		// Extract AttachFunc from ProgSpecificInfo for fentry/fexit
		var attachFunc string
		if info.Info != nil {
			switch i := info.Info.Info.(type) {
			case *pb.ProgSpecificInfo_FentryLoadInfo:
				attachFunc = i.FentryLoadInfo.FnName
			case *pb.ProgSpecificInfo_FexitLoadInfo:
				attachFunc = i.FexitLoadInfo.FnName
			}
		}

		// Create LoadSpec using the appropriate constructor (validates required fields)
		var spec bpfman.LoadSpec
		var constructErr error
		if progType.RequiresAttachFunc() {
			spec, constructErr = bpfman.NewAttachLoadSpec(objectPath, info.Name, progType, attachFunc)
		} else {
			spec, constructErr = bpfman.NewLoadSpec(objectPath, info.Name, progType)
		}
		if constructErr != nil {
			rollback()
			return nil, status.Errorf(codes.InvalidArgument, "invalid load request for %s: %v", info.Name, constructErr)
		}

		// Apply optional fields
		if req.GlobalData != nil {
			spec = spec.WithGlobalData(req.GlobalData)
		}
		if imageSource != nil {
			spec = spec.WithImageSource(imageSource)
		}

		// Map sharing: when loading multiple programs from the same image,
		// the first program creates the maps and subsequent programs share them.
		// - If req.MapOwnerId is set: use it (explicit map owner from another load)
		// - Else if this is not the first program in this request: use first program's ID
		if req.MapOwnerId != nil && *req.MapOwnerId != 0 {
			spec = spec.WithMapOwnerID(*req.MapOwnerId)
			s.logger.InfoContext(ctx, "using explicit map_owner_id from request",
				"program", info.Name,
				"map_owner_id", *req.MapOwnerId)
		} else if i > 0 && mapOwnerKernelID != 0 {
			// Subsequent programs in same request share maps with the first
			spec = spec.WithMapOwnerID(mapOwnerKernelID)
			s.logger.InfoContext(ctx, "sharing maps with first program in request",
				"program", info.Name,
				"map_owner_id", mapOwnerKernelID)
		} else if i == 0 {
			s.logger.InfoContext(ctx, "first program in request will own maps",
				"program", info.Name)
		}

		opts := manager.LoadOpts{
			UserMetadata: req.Metadata,
			Owner:        "bpfman",
		}

		loadResult, err := s.mgr.Load(ctx, spec, opts)
		if err != nil {
			rollback()
			return nil, status.Errorf(codes.Internal, "failed to load program %s: %v", info.Name, err)
		}
		loaded := loadResult

		// Track for potential rollback
		loadedKernelIDs = append(loadedKernelIDs, loaded.Spec.KernelID)

		// First program becomes the map owner for subsequent programs in this request
		if i == 0 {
			mapOwnerKernelID = loaded.Spec.KernelID
		}

		// Format LoadedAt as RFC3339 if available
		var loadedAt string
		if loaded.Status.Kernel != nil && !loaded.Status.Kernel.LoadedAt.IsZero() {
			loadedAt = loaded.Status.Kernel.LoadedAt.Format(time.RFC3339)
		}

		progInfo := &pb.ProgramInfo{
			Name:       info.Name,
			Bytecode:   req.Bytecode,
			Metadata:   req.Metadata,
			GlobalData: req.GlobalData,
			MapPinPath: loaded.Spec.Handles.MapPinPath, // maps directory computed from kernel ID
		}
		// Set MapOwnerId for dependent programs (those sharing maps with the first)
		if spec.MapOwnerID() != 0 {
			ownerID := spec.MapOwnerID()
			progInfo.MapOwnerId = &ownerID
		}

		// Build KernelProgramInfo from status
		var kernelInfo *pb.KernelProgramInfo
		if loaded.Status.Kernel != nil {
			kp := loaded.Status.Kernel
			kernelInfo = &pb.KernelProgramInfo{
				Id:            kp.ID,
				Name:          kp.Name,
				ProgramType:   uint32(loaded.Spec.Load.ProgramType()),
				LoadedAt:      loadedAt,
				Tag:           kp.Tag,
				GplCompatible: loaded.Spec.GPLCompatible,
				Jited:         kp.JitedSize > 0,
				MapIds:        kp.MapIDs,
				BtfId:         kp.BTFId,
				BytesXlated:   kp.XlatedSize,
				BytesJited:    kp.JitedSize,
				BytesMemlock:  uint32(kp.Memlock),
				VerifiedInsns: kp.VerifiedInstructions,
			}
		}

		resp.Programs = append(resp.Programs, &pb.LoadResponseInfo{
			Info:       progInfo,
			KernelInfo: kernelInfo,
		})
	}

	// Log summary of all loaded programs
	names := make([]string, len(loadedKernelIDs))
	for i, info := range req.Info {
		names[i] = info.Name
	}
	s.logger.InfoContext(ctx, "Load", "programs", names, "kernel_ids", loadedKernelIDs)

	return resp, nil
}

// Unload implements the Unload RPC method.
func (s *Server) Unload(ctx context.Context, req *pb.UnloadRequest) (*pb.UnloadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.mgr.GCIfNeeded(ctx, true); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}
	defer s.mgr.MarkMutated()

	if err := s.mgr.Unload(ctx, req.Id); err != nil {
		var notManaged bpfman.ErrProgramNotManaged
		var notFound bpfman.ErrProgramNotFound
		switch {
		case errors.As(err, &notManaged), errors.As(err, &notFound):
			return nil, status.Errorf(codes.NotFound, "%v", err)
		case errors.Is(err, store.ErrNotFound):
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", req.Id)
		default:
			return nil, status.Errorf(codes.Internal, "failed to unload program: %v", err)
		}
	}

	s.logger.InfoContext(ctx, "Unload", "program_id", req.Id)

	return &pb.UnloadResponse{}, nil
}
