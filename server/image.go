package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman/interpreter"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// PullBytecode implements the PullBytecode RPC method.
// It pre-pulls an OCI image to the local cache without loading any programs.
func (s *Server) PullBytecode(ctx context.Context, req *pb.PullBytecodeRequest) (*pb.PullBytecodeResponse, error) {
	if s.imagePuller == nil {
		return nil, status.Error(codes.Unimplemented, "OCI image pulling not configured on this server")
	}

	if req.Image == nil {
		return nil, status.Error(codes.InvalidArgument, "image is required")
	}

	// Convert proto to interpreter types
	pullPolicy := protoToPullPolicy(req.Image.ImagePullPolicy)
	ref := interpreter.ImageRef{
		URL:        req.Image.Url,
		PullPolicy: pullPolicy,
	}
	if req.Image.Username != nil && *req.Image.Username != "" {
		ref.Auth = &interpreter.ImageAuth{
			Username: *req.Image.Username,
		}
		if req.Image.Password != nil {
			ref.Auth.Password = *req.Image.Password
		}
	}

	// Pull the image (this caches it)
	_, err := s.imagePuller.Pull(ctx, ref)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to pull image %s: %v", req.Image.Url, err)
	}

	return &pb.PullBytecodeResponse{}, nil
}
