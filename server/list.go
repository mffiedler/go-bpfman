package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/kernel"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// List implements the List RPC method.
func (s *Server) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	if err := s.mgr.GCIfNeeded(ctx, false); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	scanner := bpffs.NewScanner(s.root.BPFFS().ScannerDirs())
	world, err := inspect.Snapshot(ctx, s.store, s.kernel, scanner)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to snapshot: %v", err)
	}

	var results []*pb.ListResponse_ListResult

	for _, row := range world.ManagedPrograms() {
		// Only include programs that are also in kernel
		if !row.Presence.InKernel {
			continue
		}

		prog := row.Managed
		kp := row.Kernel

		// Filter by program type if specified
		if req.ProgramType != nil && *req.ProgramType != uint32(prog.Load.ProgramType) {
			continue
		}

		// Filter by metadata if specified
		if len(req.MatchMetadata) > 0 {
			match := true
			for k, v := range req.MatchMetadata {
				if prog.Meta.Metadata[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		info := &pb.ProgramInfo{
			Name:       prog.Meta.Name,
			Bytecode:   &pb.BytecodeLocation{Location: &pb.BytecodeLocation_File{File: prog.Load.ObjectPath}},
			Metadata:   prog.Meta.Metadata,
			GlobalData: prog.Load.GlobalData,
			MapPinPath: prog.Handles.MapPinPath,
		}
		if prog.Handles.MapOwnerID != nil {
			info.MapOwnerId = prog.Handles.MapOwnerID
		}

		results = append(results, &pb.ListResponse_ListResult{
			Info: info,
			KernelInfo: &pb.KernelProgramInfo{
				Id:          row.KernelID,
				Name:        kp.Name,
				ProgramType: uint32(prog.Load.ProgramType),
				Tag:         kp.Tag,
				LoadedAt:    kp.LoadedAt.Format(time.RFC3339),
				MapIds:      kp.MapIDs,
				BtfId:       kp.BTFId,
				BytesXlated: kp.XlatedSize,
				BytesJited:  kp.JitedSize,
			},
		})
	}

	s.logger.InfoContext(ctx, "List", "programs", len(results))

	return &pb.ListResponse{Results: results}, nil
}

// Get implements the Get RPC method.
func (s *Server) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	if err := s.mgr.GCIfNeeded(ctx, false); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	scanner := bpffs.NewScanner(s.root.BPFFS().ScannerDirs())
	row, err := inspect.GetProgram(ctx, s.store, s.kernel, scanner, req.Id)
	if errors.Is(err, inspect.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "program with ID %d not found", req.Id)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "program with ID %d not found", req.Id)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get program: %v", err)
	}

	// Require program to be managed (in store)
	if !row.Presence.InStore {
		return nil, status.Errorf(codes.NotFound, "program %d not managed by bpfman", req.Id)
	}

	// Require program to be alive in kernel
	if !row.Presence.InKernel {
		return nil, status.Errorf(codes.Internal, "program %d exists in store but not in kernel (requires reconciliation)", req.Id)
	}

	prog := row.Managed
	kp := row.Kernel

	// Query store for links associated with this program
	links, err := s.store.ListLinksByProgram(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list links for program %d: %v", req.Id, err)
	}
	linkIDs := make([]uint32, 0, len(links))
	for _, link := range links {
		linkIDs = append(linkIDs, uint32(link.ID))
	}

	s.logger.InfoContext(ctx, "Get", "program_id", req.Id, "program_name", prog.Meta.Name, "links", len(linkIDs))

	info := &pb.ProgramInfo{
		Name:       prog.Meta.Name,
		Bytecode:   &pb.BytecodeLocation{Location: &pb.BytecodeLocation_File{File: prog.Load.ObjectPath}},
		Metadata:   prog.Meta.Metadata,
		GlobalData: prog.Load.GlobalData,
		MapPinPath: prog.Handles.MapPinPath,
		Links:      linkIDs,
	}
	if prog.Handles.MapOwnerID != nil {
		info.MapOwnerId = prog.Handles.MapOwnerID
	}

	// Note: GplCompatible is stored in the database at load time (from the
	// ELF license section) and retrieved here from metadata, not from the
	// kernel. The kernel doesn't expose GPL compatibility after load. The
	// field is in KernelProgramInfo because the protobuf schema is a stable
	// API that we cannot modify.
	return &pb.GetResponse{
		Info: info,
		KernelInfo: &pb.KernelProgramInfo{
			Id:            req.Id,
			Name:          kp.Name,
			ProgramType:   uint32(prog.Load.ProgramType),
			Tag:           kp.Tag,
			LoadedAt:      kp.LoadedAt.Format(time.RFC3339),
			GplCompatible: prog.Load.GPLCompatible,
			MapIds:        kp.MapIDs,
			BtfId:         kp.BTFId,
			BytesXlated:   kp.XlatedSize,
			BytesJited:    kp.JitedSize,
		},
	}, nil
}

// ListLinks implements the ListLinks RPC method.
func (s *Server) ListLinks(ctx context.Context, req *pb.ListLinksRequest) (*pb.ListLinksResponse, error) {
	if err := s.mgr.GCIfNeeded(ctx, false); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	scanner := bpffs.NewScanner(s.root.BPFFS().ScannerDirs())
	world, err := inspect.Snapshot(ctx, s.store, s.kernel, scanner)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to snapshot: %v", err)
	}

	resp := &pb.ListLinksResponse{
		Links: make([]*pb.LinkInfo, 0),
	}

	for _, row := range world.ManagedLinks() {
		// Get kernel program ID if available
		var kernelProgramID uint32
		if row.Kernel != nil {
			kernelProgramID = row.Kernel.ProgramID
		}

		// Filter by program ID if specified
		if req.ProgramId != nil && kernelProgramID != *req.ProgramId {
			continue
		}

		// Get kernel link ID if available
		var kernelLinkID uint32
		if klid := row.KernelLinkID(); klid != nil {
			kernelLinkID = *klid
		}

		resp.Links = append(resp.Links, &pb.LinkInfo{
			Summary: &pb.LinkSummary{
				KernelLinkId:    kernelLinkID,
				LinkType:        linkKindStringToProto(string(row.Kind())),
				KernelProgramId: kernelProgramID,
				PinPath:         row.PinPath(),
			},
		})
	}

	s.logger.InfoContext(ctx, "ListLinks", "links", len(resp.Links))

	return resp, nil
}

// GetLink implements the GetLink RPC method.
func (s *Server) GetLink(ctx context.Context, req *pb.GetLinkRequest) (*pb.GetLinkResponse, error) {
	if err := s.mgr.GCIfNeeded(ctx, false); err != nil {
		return nil, status.Errorf(codes.Internal, "gc: %v", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	scanner := bpffs.NewScanner(s.root.BPFFS().ScannerDirs())
	linkID := bpfman.LinkID(req.KernelLinkId)
	info, err := inspect.GetLink(ctx, s.store, s.kernel, scanner, linkID)
	if errors.Is(err, inspect.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "link with ID %d not found", req.KernelLinkId)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "link with ID %d not found", req.KernelLinkId)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get link: %v", err)
	}

	// Require link to be managed (in store)
	if !info.Presence.InStore {
		return nil, status.Errorf(codes.NotFound, "link %d not managed by bpfman", req.KernelLinkId)
	}

	// Get kernel program ID if available
	var kernelProgramID uint32
	if info.Kernel != nil {
		kernelProgramID = info.Kernel.ProgramID
	}

	s.logger.InfoContext(ctx, "GetLink", "link_id", req.KernelLinkId, "type", info.Record.Kind, "program_id", kernelProgramID)

	return &pb.GetLinkResponse{
		Link: &pb.LinkInfo{
			Summary: linkRecordToProtoSummary(info.Record, info.Kernel),
			Details: linkDetailsToProto(info.Record.Details),
		},
	}, nil
}

// linkRecordToProtoSummary converts a bpfman.LinkSpec to protobuf LinkSummary.
func linkRecordToProtoSummary(r bpfman.LinkSpec, k *kernel.Link) *pb.LinkSummary {
	// For non-synthetic links, ID is the kernel link ID
	var kernelLinkID uint32
	if !r.IsSynthetic() {
		kernelLinkID = uint32(r.ID)
	}
	// Use program ID from record (stored in DB), with kernel as verification
	kernelProgramID := r.ProgramID
	if k != nil && kernelProgramID == 0 {
		kernelProgramID = k.ProgramID
	}
	var pinPath string
	if r.PinPath != nil {
		pinPath = r.PinPath.String()
	}
	return &pb.LinkSummary{
		KernelLinkId:    kernelLinkID,
		LinkType:        linkKindToProto(r.Kind),
		KernelProgramId: kernelProgramID,
		PinPath:         pinPath,
		CreatedAt:       r.CreatedAt.Format(time.RFC3339),
	}
}

// linkKindToProto converts a bpfman.LinkKind to protobuf.
func linkKindToProto(k bpfman.LinkKind) pb.BpfmanLinkType {
	switch k {
	case bpfman.LinkKindTracepoint:
		return pb.BpfmanLinkType_LINK_TYPE_TRACEPOINT
	case bpfman.LinkKindKprobe:
		return pb.BpfmanLinkType_LINK_TYPE_KPROBE
	case bpfman.LinkKindKretprobe:
		return pb.BpfmanLinkType_LINK_TYPE_KRETPROBE
	case bpfman.LinkKindUprobe:
		return pb.BpfmanLinkType_LINK_TYPE_UPROBE
	case bpfman.LinkKindUretprobe:
		return pb.BpfmanLinkType_LINK_TYPE_URETPROBE
	case bpfman.LinkKindFentry:
		return pb.BpfmanLinkType_LINK_TYPE_FENTRY
	case bpfman.LinkKindFexit:
		return pb.BpfmanLinkType_LINK_TYPE_FEXIT
	case bpfman.LinkKindXDP:
		return pb.BpfmanLinkType_LINK_TYPE_XDP
	case bpfman.LinkKindTC:
		return pb.BpfmanLinkType_LINK_TYPE_TC
	case bpfman.LinkKindTCX:
		return pb.BpfmanLinkType_LINK_TYPE_TCX
	default:
		return pb.BpfmanLinkType_LINK_TYPE_UNSPECIFIED
	}
}

// linkKindStringToProto converts a link kind string to protobuf.
// Used when working with inspect.LinkRow which stores kind as string.
func linkKindStringToProto(k string) pb.BpfmanLinkType {
	return linkKindToProto(bpfman.LinkKind(k))
}

// linkDetailsToProto converts bpfman.LinkDetails to protobuf.
func linkDetailsToProto(d bpfman.LinkDetails) *pb.LinkDetails {
	if d == nil {
		return nil
	}

	switch details := d.(type) {
	case bpfman.TracepointDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Tracepoint{
				Tracepoint: &pb.TracepointLinkDetails{
					Group: details.Group,
					Name:  details.Name,
				},
			},
		}
	case bpfman.KprobeDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Kprobe{
				Kprobe: &pb.KprobeLinkDetails{
					FnName:   details.FnName,
					Offset:   details.Offset,
					Retprobe: details.Retprobe,
				},
			},
		}
	case bpfman.UprobeDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Uprobe{
				Uprobe: &pb.UprobeLinkDetails{
					Target:   details.Target,
					FnName:   details.FnName,
					Offset:   details.Offset,
					Pid:      details.PID,
					Retprobe: details.Retprobe,
				},
			},
		}
	case bpfman.FentryDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Fentry{
				Fentry: &pb.FentryLinkDetails{
					FnName: details.FnName,
				},
			},
		}
	case bpfman.FexitDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Fexit{
				Fexit: &pb.FexitLinkDetails{
					FnName: details.FnName,
				},
			},
		}
	case bpfman.XDPDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Xdp{
				Xdp: &pb.XDPLinkDetails{
					Interface:    details.Interface,
					Ifindex:      details.Ifindex,
					Priority:     details.Priority,
					Position:     details.Position,
					ProceedOn:    details.ProceedOn,
					Netns:        details.Netns,
					Nsid:         details.Nsid,
					DispatcherId: details.DispatcherID,
					Revision:     details.Revision,
				},
			},
		}
	case bpfman.TCDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Tc{
				Tc: &pb.TCLinkDetails{
					Interface:    details.Interface,
					Ifindex:      details.Ifindex,
					Direction:    string(details.Direction),
					Priority:     details.Priority,
					Position:     details.Position,
					ProceedOn:    details.ProceedOn,
					Netns:        details.Netns,
					Nsid:         details.Nsid,
					DispatcherId: details.DispatcherID,
					Revision:     details.Revision,
				},
			},
		}
	case bpfman.TCXDetails:
		return &pb.LinkDetails{
			Details: &pb.LinkDetails_Tcx{
				Tcx: &pb.TCXLinkDetails{
					Interface: details.Interface,
					Ifindex:   details.Ifindex,
					Direction: string(details.Direction),
					Priority:  details.Priority,
					Netns:     details.Netns,
					Nsid:      details.Nsid,
				},
			},
		}
	default:
		return nil
	}
}
