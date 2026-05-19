package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman/platform"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// PullBytecode implements the PullBytecode RPC method.
// It pre-pulls an OCI image to the local cache without loading any programs.
func (s *Server) PullBytecode(ctx context.Context, req *pb.PullBytecodeRequest) (*pb.PullBytecodeResponse, error) {
	return withReaderLock(ctx, s, func(ctx context.Context) (*pb.PullBytecodeResponse, error) {
		puller := s.mgr.ImagePuller()
		if puller == nil {
			return nil, status.Error(codes.Unimplemented, "OCI image pulling not configured on this server")
		}

		if req.Image == nil {
			return nil, status.Error(codes.InvalidArgument, "image is required")
		}

		// Convert proto to platform types
		pullPolicy, err := protoToPullPolicy(req.Image.ImagePullPolicy)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid pull policy: %v", err)
		}
		ref := platform.ImageRef{URL: req.Image.Url, PullPolicy: pullPolicy}
		if req.Image.Username != nil && *req.Image.Username != "" {
			auth := &platform.ImageAuth{
				Username: *req.Image.Username,
			}
			if req.Image.Password != nil {
				auth.Password = *req.Image.Password
			}
			ref.Auth = auth
		}

		// Pull the image (this caches it)
		_, err = puller.Pull(ctx, ref)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to pull image %s: %v", req.Image.Url, err)
		}

		return &pb.PullBytecodeResponse{}, nil
	})
}
