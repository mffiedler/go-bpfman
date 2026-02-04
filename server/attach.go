package server

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter/store"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// Attach implements the Attach RPC method.
func (s *Server) Attach(ctx context.Context, req *pb.AttachRequest) (*pb.AttachResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.mgr.GCIfNeeded(ctx, true); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}
	defer s.mgr.MarkMutated()

	if req.Attach == nil {
		return nil, status.Error(codes.InvalidArgument, "attach info is required")
	}

	var attachType string
	var resp *pb.AttachResponse
	var err error

	switch info := req.Attach.Info.(type) {
	case *pb.AttachInfo_TracepointAttachInfo:
		attachType = "tracepoint"
		resp, err = s.attachTracepoint(ctx, req.Id, info.TracepointAttachInfo)
	case *pb.AttachInfo_XdpAttachInfo:
		attachType = "xdp"
		resp, err = s.attachXDP(ctx, req.Id, info.XdpAttachInfo)
	case *pb.AttachInfo_TcAttachInfo:
		attachType = "tc"
		resp, err = s.attachTC(ctx, req.Id, info.TcAttachInfo)
	case *pb.AttachInfo_TcxAttachInfo:
		attachType = "tcx"
		resp, err = s.attachTCX(ctx, req.Id, info.TcxAttachInfo)
	case *pb.AttachInfo_KprobeAttachInfo:
		attachType = "kprobe"
		resp, err = s.attachKprobe(ctx, req.Id, info.KprobeAttachInfo)
	case *pb.AttachInfo_UprobeAttachInfo:
		attachType = "uprobe"
		resp, err = s.attachUprobe(ctx, req.Id, info.UprobeAttachInfo)
	case *pb.AttachInfo_FentryAttachInfo:
		attachType = "fentry"
		resp, err = s.attachFentry(ctx, req.Id, info.FentryAttachInfo)
	case *pb.AttachInfo_FexitAttachInfo:
		attachType = "fexit"
		resp, err = s.attachFexit(ctx, req.Id, info.FexitAttachInfo)
	default:
		return nil, status.Errorf(codes.Unimplemented, "attach type %T not yet implemented", req.Attach.Info)
	}

	if err != nil {
		return nil, err
	}

	s.logger.InfoContext(ctx, "Attach", "type", attachType, "program_id", req.Id, "link_id", resp.LinkId)

	return resp, nil
}

// attachTracepoint handles tracepoint attachment via the manager.
func (s *Server) attachTracepoint(ctx context.Context, programID uint32, info *pb.TracepointAttachInfo) (*pb.AttachResponse, error) {
	// Parse "group/name" format from tracepoint field
	parts := strings.SplitN(info.Tracepoint, "/", 2)
	if len(parts) != 2 {
		return nil, status.Errorf(codes.InvalidArgument, "tracepoint must be in 'group/name' format, got %q", info.Tracepoint)
	}
	group, name := parts[0], parts[1]

	// Construct AttachSpec with validation
	spec, err := bpfman.NewTracepointAttachSpec(programID, group, name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tracepoint attach spec: %v", err)
	}

	// Call manager with empty LinkPinPath to auto-generate
	link, err := s.mgr.AttachTracepoint(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) || errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach tracepoint: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachXDP handles XDP attachment via the manager.
func (s *Server) attachXDP(ctx context.Context, programID uint32, info *pb.XDPAttachInfo) (*pb.AttachResponse, error) {
	// Get interface index from name
	iface, err := s.netIface.InterfaceByName(info.Iface)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "interface %q: %v", info.Iface, err)
	}

	// Construct AttachSpec with validation
	spec, err := bpfman.NewXDPAttachSpec(programID, iface.Name, iface.Index)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid XDP attach spec: %v", err)
	}

	// Apply network namespace if specified
	if info.GetNetns() != "" {
		spec = spec.WithNetns(info.GetNetns())
	}

	// Call manager with empty LinkPinPath to auto-generate
	link, err := s.mgr.AttachXDP(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach XDP: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachTC handles TC attachment via the manager.
func (s *Server) attachTC(ctx context.Context, programID uint32, info *pb.TCAttachInfo) (*pb.AttachResponse, error) {
	// Parse direction at the boundary
	direction, err := bpfman.ParseTCDirection(strings.ToLower(info.Direction))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid direction %q: must be 'ingress' or 'egress'", info.Direction)
	}

	// Get interface index from name
	iface, err := s.netIface.InterfaceByName(info.Iface)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "interface %q: %v", info.Iface, err)
	}

	// Construct AttachSpec with validation
	spec, err := bpfman.NewTCAttachSpec(programID, iface.Name, iface.Index, direction)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid TC attach spec: %v", err)
	}

	// Use provided priority or default
	priority := int(info.Priority)
	if priority == 0 {
		priority = 50 // Default priority
	}
	spec = spec.WithPriority(priority)

	// Use provided proceed-on or default
	proceedOn := info.ProceedOn
	if len(proceedOn) == 0 {
		// Default: ok (0), pipe (3), dispatcher_return (30)
		proceedOn = []int32{0, 3, 30}
	}
	spec = spec.WithProceedOn(proceedOn)

	// Apply network namespace if specified
	if info.GetNetns() != "" {
		spec = spec.WithNetns(info.GetNetns())
	}

	// Call manager with empty LinkPinPath to auto-generate
	link, err := s.mgr.AttachTC(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach TC: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachTCX handles TCX attachment via the manager.
func (s *Server) attachTCX(ctx context.Context, programID uint32, info *pb.TCXAttachInfo) (*pb.AttachResponse, error) {
	// Parse direction at the boundary
	direction, err := bpfman.ParseTCDirection(strings.ToLower(info.Direction))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid direction %q: must be 'ingress' or 'egress'", info.Direction)
	}

	// Get interface index from name
	iface, err := s.netIface.InterfaceByName(info.Iface)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "interface %q: %v", info.Iface, err)
	}

	// Construct AttachSpec with validation
	spec, err := bpfman.NewTCXAttachSpec(programID, iface.Name, iface.Index, direction)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid TCX attach spec: %v", err)
	}

	// Use provided priority or default
	priority := int(info.Priority)
	if priority == 0 {
		priority = 50 // Default priority
	}
	spec = spec.WithPriority(priority)

	// Apply network namespace if specified
	if info.GetNetns() != "" {
		spec = spec.WithNetns(info.GetNetns())
	}

	// Call manager with empty LinkPinPath to auto-generate
	link, err := s.mgr.AttachTCX(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach TCX: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachKprobe handles kprobe/kretprobe attachment via the manager.
func (s *Server) attachKprobe(ctx context.Context, programID uint32, info *pb.KprobeAttachInfo) (*pb.AttachResponse, error) {
	if info.FnName == "" {
		return nil, status.Error(codes.InvalidArgument, "fn_name is required for kprobe attachment")
	}

	// Construct AttachSpec with validation
	spec, err := bpfman.NewKprobeAttachSpec(programID, info.FnName)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid kprobe attach spec: %v", err)
	}
	if info.Offset != 0 {
		spec = spec.WithOffset(info.Offset)
	}

	// Call manager - it will determine retprobe from program type
	link, err := s.mgr.AttachKprobe(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach kprobe: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachUprobe handles uprobe/uretprobe attachment via the manager.
func (s *Server) attachUprobe(ctx context.Context, programID uint32, info *pb.UprobeAttachInfo) (*pb.AttachResponse, error) {
	s.logger.DebugContext(ctx, "attachUprobe request",
		"program_id", programID,
		"target", info.Target,
		"fn_name", info.GetFnName(),
		"offset", info.Offset,
		"pid", info.GetPid(),
		"container_pid", info.GetContainerPid(),
		"container_pid_ptr", info.ContainerPid)

	if info.Target == "" {
		return nil, status.Error(codes.InvalidArgument, "target is required for uprobe attachment")
	}

	// Construct UprobeAttachSpec with validated input
	spec, err := bpfman.NewUprobeAttachSpec(programID, info.Target)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid uprobe attach spec: %v", err)
	}
	if info.GetFnName() != "" {
		spec = spec.WithFnName(info.GetFnName())
	}
	if info.Offset != 0 {
		spec = spec.WithOffset(info.Offset)
	}
	if info.ContainerPid != nil && *info.ContainerPid > 0 {
		s.logger.DebugContext(ctx, "setting container_pid on spec", "container_pid", *info.ContainerPid)
		spec = spec.WithContainerPid(*info.ContainerPid)
	}

	// Retrieve lock scope from context (set by lockInterceptor).
	// Required for container uprobes to pass lock fd to helper.
	scope := ScopeFromContext(ctx)

	// Call manager with empty linkPinPath to auto-generate
	link, err := s.mgr.AttachUprobe(ctx, scope, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach uprobe: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachFentry handles fentry attachment via the manager.
// The attach function is stored in the program metadata from load time.
func (s *Server) attachFentry(ctx context.Context, programID uint32, info *pb.FentryAttachInfo) (*pb.AttachResponse, error) {
	// Construct FentryAttachSpec with validated input
	spec, err := bpfman.NewFentryAttachSpec(programID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid fentry attach spec: %v", err)
	}

	// Call manager with empty linkPinPath to auto-generate.
	// The manager will retrieve the attach function from the stored program metadata.
	link, err := s.mgr.AttachFentry(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach fentry: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// attachFexit handles fexit attachment via the manager.
// The attach function is stored in the program metadata from load time.
func (s *Server) attachFexit(ctx context.Context, programID uint32, info *pb.FexitAttachInfo) (*pb.AttachResponse, error) {
	// Construct FexitAttachSpec with validated input
	spec, err := bpfman.NewFexitAttachSpec(programID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid fexit attach spec: %v", err)
	}

	// Call manager with empty linkPinPath to auto-generate.
	// The manager will retrieve the attach function from the stored program metadata.
	link, err := s.mgr.AttachFexit(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		var notFound bpfman.ErrProgramNotFound
		if errors.As(err, &notFound) {
			return nil, status.Errorf(codes.NotFound, "program with ID %d not found", programID)
		}
		return nil, status.Errorf(codes.Internal, "attach fexit: %v", err)
	}

	return &pb.AttachResponse{
		LinkId: uint32(link.Spec.ID),
	}, nil
}

// Detach implements the Detach RPC method.
func (s *Server) Detach(ctx context.Context, req *pb.DetachRequest) (*pb.DetachResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.mgr.GCIfNeeded(ctx, true); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}
	defer s.mgr.MarkMutated()

	if err := s.mgr.Detach(ctx, bpfman.LinkID(req.LinkId)); err != nil {
		var notManaged bpfman.ErrLinkNotManaged
		var notFound bpfman.ErrLinkNotFound
		switch {
		case errors.As(err, &notManaged), errors.As(err, &notFound):
			return nil, status.Errorf(codes.NotFound, "%v", err)
		case errors.Is(err, store.ErrNotFound):
			return nil, status.Errorf(codes.NotFound, "link with ID %d not found", req.LinkId)
		default:
			return nil, status.Errorf(codes.Internal, "detach link: %v", err)
		}
	}

	s.logger.InfoContext(ctx, "Detach", "link_id", req.LinkId)

	return &pb.DetachResponse{}, nil
}
