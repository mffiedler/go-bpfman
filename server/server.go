package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/frobware/go-bpfman/config"
	driver "github.com/frobware/go-bpfman/csi"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/fs/runtime"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/image/oci"
	"github.com/frobware/go-bpfman/platform/image/verify"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
	pb "github.com/frobware/go-bpfman/server/pb"
)

const (
	// DefaultCSIDriverName is the default CSI driver name.
	// Uses csi.bpfman.io for compatibility with bpfman-operator.
	DefaultCSIDriverName = "csi.bpfman.io"
	// DefaultCSIVersion is the default CSI driver version.
	DefaultCSIVersion = "0.1.0"
)

// NetIfaceResolver resolves network interfaces by name.
// This interface enables testing without real network interfaces.
type NetIfaceResolver interface {
	InterfaceByName(name string) (*net.Interface, error)
}

// DefaultNetIfaceResolver uses the standard library net package.
type DefaultNetIfaceResolver struct{}

func (DefaultNetIfaceResolver) InterfaceByName(name string) (*net.Interface, error) {
	return net.InterfaceByName(name)
}

// RunConfig configures the server daemon.
type RunConfig struct {
	Layout       fs.Layout
	ImageCache   fs.EnsuredImageCache // Capability token proving cache directory exists
	TCPAddress   string               // Optional TCP address (e.g., ":50051") for remote access
	CSISupport   bool
	PprofAddress string // Optional address for pprof HTTP server (e.g., "localhost:2026")
	SocketPath   string // Optional override for Unix socket path (defaults to layout.SocketPath())
	Logger       *slog.Logger
	Config       config.Config
}

// Run starts the bpfman daemon with the given configuration.
// This is the main entry point for the serve command.
// The context is used for cancellation - when cancelled, the server shuts down gracefully.
func Run(ctx context.Context, cfg RunConfig) error {
	layout := cfg.Layout

	// Capture the process's startup netns inode now, before any
	// goroutine that might call setns has been spawned. The
	// ns/netns package caches this under a sync.Once and every
	// later call to CurrentNSID / NSID("") returns the captured
	// value. Priming on the calling goroutine -- which is
	// whatever drove main() to here, still on a thread that has
	// not switched namespaces -- locks in a known-good value.
	// Subsequent manager attaches that resolve an empty netns
	// path to "root" via the NSID("") -> processNSID fallback
	// then see the right inode regardless of which thread the
	// request landed on. Failure here means /proc/self/ns/net is
	// unreadable, which makes the rest of the daemon meaningless
	// -- panic so a stack trace makes the startup failure
	// obvious rather than a generic error from every later
	// attach. Symmetric with the equivalent prime in the e2e
	// TestMain.
	if _, err := netns.CurrentNSID(); err != nil {
		panic(fmt.Errorf("server: prime ns/netns capture: %v", err))
	}

	logger := cfg.Logger
	// Wrap with context-aware handler to extract op_id from context.
	// This must happen at the server level since op_id is generated here.
	logger = manager.WithOpIDHandler(logger)

	// Open shared SQLite store
	dbPath := layout.DBPath()
	st, err := sqlite.New(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("failed to open store at %s: %w", dbPath, err)
	}
	defer st.Close()

	// Create kernel adapter
	kernel := ebpf.New(ebpf.WithLogger(logger))

	// Ensure runtime directories and bpffs mount
	ensuredRuntime, err := runtime.New(layout, runtime.RealMounter{}, logger)
	if err != nil {
		return fmt.Errorf("ensure runtime: %w", err)
	}

	// Build signature verifier based on config
	var verifier platform.SignatureVerifier
	if cfg.Config.Signing.ShouldVerify() {
		logger.Info("signature verification enabled")
		verifier = verify.Cosign(
			verify.WithLogger(logger),
			verify.WithAllowUnsigned(cfg.Config.Signing.AllowUnsigned),
		)
	} else {
		logger.Info("signature verification disabled")
		verifier = verify.NoSign()
	}

	// Create image puller for OCI images
	puller, err := oci.NewPuller(
		cfg.ImageCache,
		oci.WithLogger(logger),
		oci.WithVerifier(verifier),
	)
	if err != nil {
		return fmt.Errorf("failed to create image puller: %w", err)
	}

	// Create manager for orchestrating store + kernel operations.
	// The manager is needed by CSI for reconciled program lookups.
	mgr, err := manager.New(ensuredRuntime, puller, st, kernel, ebpf.NewProgramDiscoverer(), logger)
	if err != nil {
		return fmt.Errorf("failed to create manager: %w", err)
	}

	// Start CSI driver if enabled, and arrange for it to stop on
	// root context cancellation. When CSI is disabled we spawn no
	// shutdown goroutine; the gRPC server's own ctx.Done handler
	// drives gRPC shutdown directly.
	if cfg.CSISupport {
		for _, dir := range layout.CSIDirs() {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("create CSI directory %s: %w", dir, err)
			}
		}

		nodeID, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname for node ID: %w", err)
		}

		csiSocketPath := layout.CSISocketPath()
		csiDriver := driver.New(
			DefaultCSIDriverName,
			DefaultCSIVersion,
			nodeID,
			"unix://"+csiSocketPath,
			logger,
			driver.WithProgramFinder(mgr),
			driver.WithKernel(kernel),
		)

		go func() {
			logger.Info("starting CSI driver",
				"socket", csiSocketPath,
				"driver", DefaultCSIDriverName,
			)
			if err := csiDriver.Run(); err != nil {
				logger.Error("CSI driver failed", "error", err)
			}
		}()

		go func() {
			<-ctx.Done()
			logger.Info("stopping CSI driver")
			csiDriver.Stop()
		}()
	}

	// Start pprof HTTP server if configured.
	if cfg.PprofAddress != "" {
		pprofListener, err := net.Listen("tcp", cfg.PprofAddress)
		if err != nil {
			return fmt.Errorf("pprof listen on %s: %w", cfg.PprofAddress, err)
		}
		pprofServer := &http.Server{}
		logger.Info("pprof HTTP server listening", "address", pprofListener.Addr().String())
		go func() {
			if err := pprofServer.Serve(pprofListener); err != nil && err != http.ErrServerClosed {
				logger.Error("pprof HTTP server failed", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			pprofServer.Close()
		}()
	} else {
		logger.Info("pprof HTTP server disabled")
	}

	// Start bpfman gRPC server
	srv := &Server{
		layout:   layout,
		netIface: DefaultNetIfaceResolver{},
		mgr:      mgr,
		logger:   logger.With("component", "server"),
	}

	// Use override socket path if provided, otherwise use default from layout
	socketPath := cfg.SocketPath
	if socketPath == "" {
		socketPath = layout.SocketPath()
	}

	return srv.serve(ctx, socketPath, cfg.TCPAddress)
}

// Server implements the bpfman gRPC service.
//
// The server runs on the cross-process writer flock alone, with no
// in-process serialisation. Mutating handlers (Unload, Attach,
// Detach) wrap their body in withWriterLock to acquire the
// file-based writer lock from the lock package; read handlers
// (List, Get, ListLinks, PullBytecode) run lockless and rely on
// the store and kernel adapter for safe concurrent access. The
// Load handler also takes no server-level lock; routing Load
// through withWriterLock would self-deadlock against the manager's
// own conditional flock acquisition for LIBBPF_PIN_BY_NAME loads.
// Cross-process serialisation for shared-map loads is the
// manager's responsibility (see Manager.Load and
// docs/PLAN-load-lockless.md).
type Server struct {
	pb.UnimplementedBpfmanServer

	layout    fs.Layout
	netIface  NetIfaceResolver
	mgr       *manager.Manager
	logger    *slog.Logger
	opCounter atomic.Uint64
}

// New creates a server with the provided dependencies.
// The manager must be created by the caller - use manager.New() with
// appropriate mounter (RealMounter for production, NoOpMounter for tests).
// The manager should include an ImagePuller if OCI image loading is needed.
func New(layout fs.Layout, netIface NetIfaceResolver, mgr *manager.Manager, logger *slog.Logger) *Server {
	// Wrap with context-aware handler to extract op_id from context.
	logger = manager.WithOpIDHandler(logger)
	return &Server{
		layout:   layout,
		netIface: netIface,
		mgr:      mgr,
		logger:   logger.With("component", "server"),
	}
}

// serve starts the gRPC server on the given socket path and optionally on TCP.
func (s *Server) serve(ctx context.Context, socketPath, tcpAddr string) error {
	// Ensure socket directory exists
	socketDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	// Remove existing socket file
	if err := os.RemoveAll(socketPath); err != nil {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create Unix socket listener
	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}
	defer unixListener.Close()

	// Set socket permissions
	if err := os.Chmod(socketPath, 0660); err != nil {
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(s.rpcInterceptor()),
	)
	pb.RegisterBpfmanServer(grpcServer, s)

	// Track errors from serving goroutines
	errChan := make(chan error, 2)

	// Start Unix socket server
	go func() {
		s.logger.InfoContext(ctx, "bpfman gRPC server listening", "socket", socketPath)
		if err := grpcServer.Serve(unixListener); err != nil {
			errChan <- fmt.Errorf("unix socket server: %w", err)
		}
	}()

	// Optionally start TCP listener for remote access
	if tcpAddr != "" {
		tcpListener, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			grpcServer.GracefulStop()
			return fmt.Errorf("failed to listen on TCP %s: %w", tcpAddr, err)
		}

		go func() {
			s.logger.InfoContext(ctx, "bpfman gRPC server listening", "tcp", tcpAddr)
			if err := grpcServer.Serve(tcpListener); err != nil {
				errChan <- fmt.Errorf("tcp server: %w", err)
			}
		}()
	}

	// Handle context cancellation for graceful shutdown
	go func() {
		<-ctx.Done()
		s.logger.InfoContext(ctx, "shutting down gRPC server")
		grpcServer.GracefulStop()
	}()

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		return nil
	case err := <-errChan:
		return err
	}
}

// rpcInterceptor returns a gRPC unary interceptor that assigns an
// operation ID to each request and logs handler errors centrally.
// Per-request locking is the handler's own responsibility -- see
// withWriterLock.
func (s *Server) rpcInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		opID := s.opCounter.Add(1)
		ctx = manager.ContextWithOpID(ctx, opID)

		resp, err := handler(ctx, req)
		if err != nil {
			s.logger.ErrorContext(ctx, "grpc error", "op_id", opID, "method", info.FullMethod, "error", err)
		}
		return resp, err
	}
}

// withWriterLock runs fn under the cross-process writer flock.
// The acquired writer scope is passed to fn so handlers that need
// to forward it (container uprobes passing the lock fd to the
// bpfman-ns helper) can do so directly, without a context
// round-trip.
func withWriterLock[T any](ctx context.Context, s *Server, fn func(context.Context, lock.WriterScope) (T, error)) (T, error) {
	var resp T
	err := lock.RunWithTiming(ctx, s.layout.LockPath(), s.logger, func(ctx context.Context, writeLock lock.WriterScope) error {
		var fnErr error
		resp, fnErr = fn(ctx, writeLock)
		return fnErr
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return resp, nil
}
