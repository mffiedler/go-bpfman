//go:build e2e

package e2e

import "embed"

// BpfFS embeds the compiled BPF object tree under testdata/bpf/.
// The declaration lives in the e2e package -- rather than in
// e2e/testbpf or e2e/grpc -- because Go's embed directive
// forbids ".." in patterns, so the directive must come from a
// .go file whose package directory sits above testdata/bpf/.
// Both e2e.test (this package's own tests) and e2e-grpc.test
// (the sibling e2e/grpc package) consume BpfFS via
// testbpf.Materialise, so neither side carries its own copy of
// the .bpf.o tree.
//
//go:embed testdata/bpf/*.bpf.o
var BpfFS embed.FS
