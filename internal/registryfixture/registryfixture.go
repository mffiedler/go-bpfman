// Package registryfixture owns the anonymous OCI registry fixture used by
// bpfman-shell image e2e scripts.
package registryfixture

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/go-containerregistry/pkg/registry"
)

const (
	// RegistryAlias is the stable host name scripts use before the shell
	// rewrites it to the live loopback registry.
	RegistryAlias = "bpfman-e2e-registry.example.com"
	// RepositoryPrefix is the repository namespace reserved for e2e images.
	RepositoryPrefix = "bpfman-e2e"
	// RegistryEnv overrides the backing registry host for this process.
	RegistryEnv = "BPFMAN_E2E_IMAGE_REGISTRY"
)

var (
	lazyRegistryMu sync.Mutex
	lazyRegistry   *httptest.Server
	lazyHost       string
	refSeq         atomic.Uint64
)

// Host returns the backing registry host. An explicit RegistryEnv wins; when it
// is unset a process-local anonymous loopback registry is started lazily.
func Host() (string, error) {
	if registryHost := os.Getenv(RegistryEnv); registryHost != "" {
		return normaliseHost(registryHost)
	}

	lazyRegistryMu.Lock()
	defer lazyRegistryMu.Unlock()

	if lazyHost != "" {
		return lazyHost, nil
	}

	server, host, err := newRegistry()
	if err != nil {
		return "", err
	}
	lazyRegistry = server
	lazyHost = host
	return lazyHost, nil
}

// URL returns the backing registry URL. Loopback hosts use http because the
// fixture registry is plain HTTP; non-loopback overrides default to https.
func URL() (string, error) {
	if registryHost := os.Getenv(RegistryEnv); registryHost != "" {
		if strings.HasPrefix(registryHost, "http://") || strings.HasPrefix(registryHost, "https://") {
			host, err := normaliseHost(registryHost)
			if err != nil {
				return "", err
			}
			u, _ := url.Parse(registryHost)
			return u.Scheme + "://" + host, nil
		}
	}
	host, err := Host()
	if err != nil {
		return "", err
	}
	scheme := "https"
	if isLoopbackRegistry(host) {
		scheme = "http"
	}
	return scheme + "://" + host, nil
}

// StartShared starts an anonymous registry whose lifecycle is owned by the
// caller. It is used by the e2e runner so child bpfman-shell processes share
// one registry through RegistryEnv.
func StartShared() (string, func(), error) {
	server, host, err := newRegistry()
	if err != nil {
		return "", nil, err
	}
	return host, server.Close, nil
}

// Close releases only the lazily-started registry owned by this process.
func Close() {
	lazyRegistryMu.Lock()
	defer lazyRegistryMu.Unlock()

	if lazyRegistry != nil {
		lazyRegistry.Close()
		lazyRegistry = nil
		lazyHost = ""
	}
}

// Ref returns a unique alias reference in the e2e repository namespace.
func Ref(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("registry ref: NAME must not be empty")
	}
	return RegistryAlias + "/" + RepositoryPrefix + "/" + SanitiseComponent(name) + ":" + UniqueSuffix(), nil
}

// UniqueSuffix returns a process-local image tag suffix.
func UniqueSuffix() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), refSeq.Add(1))
}

// SanitiseComponent normalises a string for use as an OCI repository component.
func SanitiseComponent(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "bytecode"
	}
	return out
}

func newRegistry() (*httptest.Server, string, error) {
	server := httptest.NewServer(registry.New(
		registry.Logger(log.New(io.Discard, "", 0)),
	))
	u, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		return nil, "", fmt.Errorf("start e2e image registry: %w", err)
	}
	return server, u.Host, nil
}

func normaliseHost(s string) (string, error) {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", RegistryEnv, err)
		}
		if u.Host == "" || u.Path != "" && u.Path != "/" {
			return "", fmt.Errorf("%s must name a registry host, got %q", RegistryEnv, s)
		}
		return u.Host, nil
	}
	host := strings.TrimSuffix(s, "/")
	if host == "" || strings.Contains(host, "/") {
		return "", fmt.Errorf("%s must name a registry host, got %q", RegistryEnv, s)
	}
	return host, nil
}

func isLoopbackRegistry(registryHost string) bool {
	host := registryHost
	if h, _, err := net.SplitHostPort(registryHost); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
