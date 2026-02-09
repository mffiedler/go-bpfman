package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/kernel"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// List implements the List RPC method.
func (s *Server) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	var opts []bpfman.ListOption
	if req.ProgramType != nil {
		opts = append(opts, bpfman.WithTypes(bpfman.ProgramType(*req.ProgramType)))
	}
	if len(req.MatchMetadata) > 0 {
		opts = append(opts, bpfman.MatchingLabels(req.MatchMetadata))
	}

	result, err := s.mgr.ListPrograms(ctx, opts...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list programs: %v", err)
	}

	var results []*pb.ListResponse_ListResult
	for _, prog := range result.Programs {
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
			MapPinPath: prog.Record.Handles.MapPinPath,
		}
		if prog.Record.Handles.MapOwnerID != nil {
			info.MapOwnerId = prog.Record.Handles.MapOwnerID
		}

		results = append(results, &pb.ListResponse_ListResult{
			Info: info,
			KernelInfo: &pb.KernelProgramInfo{
				Id:          prog.Record.KernelID,
				Name:        kp.Name,
				ProgramType: uint32(prog.Record.Load.ProgramType()),
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
	prog, err := s.mgr.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "program with ID %d not found", req.Id)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get program: %v", err)
	}

	kp := prog.Status.Kernel
	if kp == nil {
		return nil, status.Errorf(codes.Internal, "program %d exists in store but not in kernel (requires reconciliation)", req.Id)
	}

	// Link IDs from the program's status
	linkIDs := make([]uint32, 0, len(prog.Status.Links))
	for _, link := range prog.Status.Links {
		linkIDs = append(linkIDs, uint32(link.Record.ID))
	}

	s.logger.InfoContext(ctx, "Get", "program_id", req.Id, "program_name", prog.Record.Meta.Name, "links", len(linkIDs))

	info := &pb.ProgramInfo{
		Name:       prog.Record.Meta.Name,
		Bytecode:   &pb.BytecodeLocation{Location: &pb.BytecodeLocation_File{File: prog.Record.Load.ObjectPath()}},
		Metadata:   prog.Record.Meta.Metadata,
		GlobalData: prog.Record.Load.GlobalData(),
		MapPinPath: prog.Record.Handles.MapPinPath,
		Links:      linkIDs,
	}
	if prog.Record.Handles.MapOwnerID != nil {
		info.MapOwnerId = prog.Record.Handles.MapOwnerID
	}

	return &pb.GetResponse{
		Info: info,
		KernelInfo: &pb.KernelProgramInfo{
			Id:            req.Id,
			Name:          kp.Name,
			ProgramType:   uint32(prog.Record.Load.ProgramType()),
			Tag:           kp.Tag,
			LoadedAt:      kp.LoadedAt.Format(time.RFC3339),
			GplCompatible: prog.Record.GPLCompatible,
			MapIds:        kp.MapIDs,
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
		opts = append(opts, bpfman.WithProgramID(*req.ProgramId))
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
		if !record.IsSynthetic() {
			kernelLinkID = uint32(record.ID)
		}

		resp.Links = append(resp.Links, &pb.LinkInfo{
			Summary: &pb.LinkSummary{
				KernelLinkId:    kernelLinkID,
				LinkType:        linkKindToProto(record.Kind),
				KernelProgramId: record.ProgramID,
			},
		})
	}

	s.logger.InfoContext(ctx, "ListLinks", "links", len(resp.Links))
	return resp, nil
}

// GetLink implements the GetLink RPC method.
func (s *Server) GetLink(ctx context.Context, req *pb.GetLinkRequest) (*pb.GetLinkResponse, error) {
	linkID := bpfman.LinkID(req.KernelLinkId)
	info, err := s.mgr.GetLinkInfo(ctx, linkID)
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

// linkRecordToProtoSummary converts a bpfman.LinkRecord to protobuf LinkSummary.
func linkRecordToProtoSummary(r bpfman.LinkRecord, k *kernel.Link) *pb.LinkSummary {
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
