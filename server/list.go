package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/inspect"
	"github.com/bpfman/bpfman/kernel"
	"github.com/bpfman/bpfman/manager"
	"github.com/bpfman/bpfman/platform"
	pb "github.com/bpfman/bpfman/server/pb"
)

// List implements the List RPC method.
func (s *Server) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	var opts []bpfman.ListOption
	if req.ProgramType != nil {
		pt, err := protoToBpfmanType(pb.BpfmanProgramType(*req.ProgramType))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid program type: %v", err)
		}

		opts = append(opts, bpfman.WithTypes(pt))
	}
	if len(req.MatchMetadata) > 0 {
		opts = append(opts, bpfman.MatchingLabels(req.MatchMetadata))
	}

	result, err := s.mgr.ListPrograms(ctx, opts...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list programs: %v", err)
	}

	var results []*pb.ListResponse_ListResult
	for _, prog := range result {
		// Only include programs that are also in kernel
		if prog.Status.Kernel == nil {
			continue
		}
		kp := prog.Status.Kernel

		info := &pb.ProgramInfo{
			Name:       prog.Record.Meta.Name,
			Bytecode:   &pb.BytecodeLocation{Location: &pb.BytecodeLocation_File{File: prog.Record.Load.ObjectPath()}},
			Metadata:   prog.Record.Meta.Metadata,
			GlobalData: prog.Record.Load.GlobalData(),
			MapPinPath: prog.Record.Handles.MapsDir.String(),
			MapUsedBy:  programIDsToStrings(prog.Status.MapUsedBy),
		}
		if prog.Record.Handles.MapOwnerID != nil {
			v := uint32(*prog.Record.Handles.MapOwnerID)
			info.MapOwnerId = &v
		}

		mapIDs := make([]uint32, len(kp.MapIDs))
		for i, id := range kp.MapIDs {
			mapIDs[i] = uint32(id)
		}

		results = append(results, &pb.ListResponse_ListResult{
			Info: info,
			KernelInfo: &pb.KernelProgramInfo{
				Id:          uint32(prog.Record.ProgramID),
				Name:        kp.Name,
				ProgramType: bpfmanTypeToProto(prog.Record.Load.ProgramType()),
				Tag:         kp.Tag,
				LoadedAt:    kp.LoadedAt.Format(time.RFC3339),
				MapIds:      mapIDs,
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
	prog, err := s.mgr.Get(ctx, kernel.ProgramID(req.Id))
	if errors.Is(err, platform.ErrRecordNotFound) {
		return nil, status.Errorf(codes.NotFound, "program with ID %d not found", req.Id)
	}
	var reconcileErr manager.ErrProgramRequiresReconciliation
	if errors.As(err, &reconcileErr) {
		return nil, status.Error(codes.Internal, reconcileErr.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get program: %v", err)
	}

	kp := prog.Status.Kernel

	// Managed link IDs from the program's status.
	linkIDs := make([]uint32, 0, len(prog.Status.Links))
	for _, link := range prog.Status.Links {
		linkIDs = append(linkIDs, grpcLinkID(link.Record.ID))
	}

	s.logger.InfoContext(ctx, "Get", "program_id", req.Id, "program_name", prog.Record.Meta.Name, "links", len(linkIDs))

	info := &pb.ProgramInfo{
		Name:       prog.Record.Meta.Name,
		Bytecode:   &pb.BytecodeLocation{Location: &pb.BytecodeLocation_File{File: prog.Record.Load.ObjectPath()}},
		Metadata:   prog.Record.Meta.Metadata,
		GlobalData: prog.Record.Load.GlobalData(),
		MapPinPath: prog.Record.Handles.MapsDir.String(),
		MapUsedBy:  programIDsToStrings(prog.Status.MapUsedBy),
		Links:      linkIDs,
	}
	if prog.Record.Handles.MapOwnerID != nil {
		v := uint32(*prog.Record.Handles.MapOwnerID)
		info.MapOwnerId = &v
	}

	mapIDs := make([]uint32, len(kp.MapIDs))
	for i, id := range kp.MapIDs {
		mapIDs[i] = uint32(id)
	}

	return &pb.GetResponse{
		Info: info,
		KernelInfo: &pb.KernelProgramInfo{
			Id:            req.Id,
			Name:          kp.Name,
			ProgramType:   bpfmanTypeToProto(prog.Record.Load.ProgramType()),
			Tag:           kp.Tag,
			LoadedAt:      kp.LoadedAt.Format(time.RFC3339),
			GplCompatible: prog.Record.GPLCompatible,
			MapIds:        mapIDs,
			BtfId:         kp.BTFId,
			BytesXlated:   kp.XlatedSize,
			BytesJited:    kp.JitedSize,
		},
	}, nil
}

// ListLinks implements the ListLinks RPC method.
func (s *Server) ListLinks(ctx context.Context, req *pb.ListLinksRequest) (*pb.ListLinksResponse, error) {
	var opts []bpfman.LinkListOption
	if req.ProgramId != nil {
		opts = append(opts, bpfman.WithProgramID(kernel.ProgramID(*req.ProgramId)))
	}

	records, err := s.mgr.ListLinks(ctx, opts...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list links: %v", err)
	}

	resp := &pb.ListLinksResponse{
		Links: make([]*pb.LinkInfo, 0, len(records)),
	}

	for _, record := range records {
		var kernelLinkID uint32
		if record.KernelLinkID != nil {
			kernelLinkID = uint32(*record.KernelLinkID)
		}

		resp.Links = append(resp.Links, &pb.LinkInfo{
			Summary: &pb.LinkSummary{
				KernelLinkId:    kernelLinkID,
				LinkType:        linkKindToProto(record.Kind),
				KernelProgramId: uint32(record.ProgramID),
			},
		})
	}

	s.logger.InfoContext(ctx, "ListLinks", "links", len(resp.Links))
	return resp, nil
}

// GetLink implements the GetLink RPC method.
func (s *Server) GetLink(ctx context.Context, req *pb.GetLinkRequest) (*pb.GetLinkResponse, error) {
	// The legacy protobuf field is named kernel_link_id; the server
	// uses bpfman-managed link handles, so interpret this field as the
	// bpfman LinkID at the boundary.
	linkID := bpfman.LinkID(req.KernelLinkId)
	info, err := s.mgr.GetLinkInfo(ctx, linkID)
	if errors.Is(err, inspect.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "link with ID %d not found", req.KernelLinkId)
	}
	if errors.Is(err, platform.ErrRecordNotFound) {
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
		kernelProgramID = uint32(info.Kernel.ProgramID)
	}

	s.logger.InfoContext(ctx, "GetLink", "bpfman_link_id", req.KernelLinkId, "type", info.Record.Kind, "program_id", kernelProgramID)

	return &pb.GetLinkResponse{
		Link: &pb.LinkInfo{
			Summary: linkRecordToProtoSummary(info.Record, info.Kernel),
			Details: linkDetailsToProto(info.Record.Details),
		},
	}, nil
}

// linkRecordToProtoSummary converts a bpfman.LinkRecord to protobuf LinkSummary.
func linkRecordToProtoSummary(r bpfman.LinkRecord, k *kernel.Link) *pb.LinkSummary {
	var kernelLinkID uint32
	if r.KernelLinkID != nil {
		kernelLinkID = uint32(*r.KernelLinkID)
	}
	// Use program ID from record (stored in DB), with kernel as verification
	kernelProgramID := uint32(r.ProgramID)
	if k != nil && kernelProgramID == 0 {
		kernelProgramID = uint32(k.ProgramID)
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
					DispatcherId: uint32(details.DispatcherID),
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
					Direction:    details.Direction.String(),
					Priority:     details.Priority,
					Position:     details.Position,
					ProceedOn:    details.ProceedOn,
					Netns:        details.Netns,
					Nsid:         details.Nsid,
					DispatcherId: uint32(details.DispatcherID),
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
					Direction: details.Direction.String(),
					Priority:  details.Priority,
					Position:  details.Position,
					Netns:     details.Netns,
					Nsid:      details.Nsid,
				},
			},
		}
	default:
		return nil
	}
}
