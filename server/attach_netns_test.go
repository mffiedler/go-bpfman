package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman/kernel"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// recordingResolver records the arguments of the most recent
// InterfaceByName call and returns a fixed result. Returning an error
// lets the attach handlers be exercised in isolation: each one resolves
// the interface before touching the manager, so an error here makes the
// handler return without a real manager wired in.
type recordingResolver struct {
	gotName  string
	gotNetns string
	iface    *net.Interface
	err      error
}

func (r *recordingResolver) InterfaceByName(name, netnsPath string) (*net.Interface, error) {
	r.gotName = name
	r.gotNetns = netnsPath
	return r.iface, r.err
}

// TestAttachResolvesInterfaceInRequestedNetns asserts that the XDP, TC
// and TCX attach handlers resolve the interface name in the network
// namespace named on the request, not in the daemon's own namespace. A
// namespaced interface such as a pod's "eth0" does not exist in the host
// namespace; resolving it there fails with "route ip+net: no such
// network interface" and is the regression this guards against.
func TestAttachResolvesInterfaceInRequestedNetns(t *testing.T) {
	t.Parallel()

	const (
		iface = "eth0"
		netns = "/proc/1234/ns/net"
	)
	resolveErr := errors.New("no such network interface")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name   string
		attach func(s *Server, r *recordingResolver) (*pb.AttachResponse, error)
	}{
		{
			name: "xdp",
			attach: func(s *Server, _ *recordingResolver) (*pb.AttachResponse, error) {
				return s.attachXDP(context.Background(), nil, kernel.ProgramID(1),
					&pb.XDPAttachInfo{Iface: iface, Netns: ptr(netns)})
			},
		},
		{
			name: "tc",
			attach: func(s *Server, _ *recordingResolver) (*pb.AttachResponse, error) {
				return s.attachTC(context.Background(), nil, kernel.ProgramID(1),
					&pb.TCAttachInfo{Iface: iface, Direction: "ingress", Netns: ptr(netns)})
			},
		},
		{
			name: "tcx",
			attach: func(s *Server, _ *recordingResolver) (*pb.AttachResponse, error) {
				return s.attachTCX(context.Background(), nil, kernel.ProgramID(1),
					&pb.TCXAttachInfo{Iface: iface, Direction: "ingress", Netns: ptr(netns)})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &recordingResolver{err: resolveErr}
			s := &Server{netIface: r, logger: logger}

			_, err := tt.attach(s, r)

			if r.gotName != iface {
				t.Errorf("resolved name = %q, want %q", r.gotName, iface)
			}
			if r.gotNetns != netns {
				t.Errorf("resolved in netns %q, want %q", r.gotNetns, netns)
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("error code = %v, want InvalidArgument (err: %v)", status.Code(err), err)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
