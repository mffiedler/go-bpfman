package bpfman_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/frobware/go-bpfman"
)

func TestLinkListOptions_WithKinds(t *testing.T) {
	opts := bpfman.ApplyLinkListOptions(bpfman.WithKinds(bpfman.LinkKindXDP, bpfman.LinkKindTC))

	xdpLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindXDP}
	tcLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindTC}
	kprobeLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindKprobe}

	assert.True(t, opts.Matches(xdpLink), "should match XDP")
	assert.True(t, opts.Matches(tcLink), "should match TC")
	assert.False(t, opts.Matches(kprobeLink), "should not match kprobe")
}

func TestLinkListOptions_WithProgramID(t *testing.T) {
	opts := bpfman.ApplyLinkListOptions(bpfman.WithProgramID(123))

	matchingLink := &bpfman.LinkSpec{ProgramID: 123}
	nonMatchingLink := &bpfman.LinkSpec{ProgramID: 456}

	assert.True(t, opts.Matches(matchingLink), "should match program 123")
	assert.False(t, opts.Matches(nonMatchingLink), "should not match program 456")
}

func TestLinkListOptions_Combined(t *testing.T) {
	opts := bpfman.ApplyLinkListOptions(
		bpfman.WithKinds(bpfman.LinkKindXDP),
		bpfman.WithProgramID(123),
	)

	matchingLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindXDP, ProgramID: 123}
	wrongKind := &bpfman.LinkSpec{Kind: bpfman.LinkKindTC, ProgramID: 123}
	wrongProgram := &bpfman.LinkSpec{Kind: bpfman.LinkKindXDP, ProgramID: 456}

	assert.True(t, opts.Matches(matchingLink), "should match XDP + program 123")
	assert.False(t, opts.Matches(wrongKind), "should not match TC")
	assert.False(t, opts.Matches(wrongProgram), "should not match wrong program")
}

func TestLinkListOptions_EmptyMatchesAll(t *testing.T) {
	opts := bpfman.ApplyLinkListOptions()

	anyLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindKprobe, ProgramID: 999}
	assert.True(t, opts.Matches(anyLink), "empty options should match all")
}

func TestLinkListOptions_WithKinds_Empty(t *testing.T) {
	// Empty kinds should match all
	opts := bpfman.ApplyLinkListOptions(bpfman.WithKinds())

	link := &bpfman.LinkSpec{Kind: bpfman.LinkKindXDP}
	assert.True(t, opts.Matches(link), "empty kinds should match all links")
}

func TestLinkListOptions_MultipleWithKinds(t *testing.T) {
	// Calling WithKinds multiple times should accumulate
	opts := bpfman.ApplyLinkListOptions(
		bpfman.WithKinds(bpfman.LinkKindXDP),
		bpfman.WithKinds(bpfman.LinkKindKprobe),
	)

	xdpLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindXDP}
	kprobeLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindKprobe}
	tcLink := &bpfman.LinkSpec{Kind: bpfman.LinkKindTC}

	assert.True(t, opts.Matches(xdpLink), "should match XDP")
	assert.True(t, opts.Matches(kprobeLink), "should match Kprobe")
	assert.False(t, opts.Matches(tcLink), "should not match TC")
}

func TestLinkKindNames(t *testing.T) {
	names := bpfman.LinkKindNames()

	assert.Contains(t, names, "xdp")
	assert.Contains(t, names, "tc")
	assert.Contains(t, names, "tcx")
	assert.Contains(t, names, "kprobe")
	assert.Contains(t, names, "kretprobe")
	assert.Contains(t, names, "uprobe")
	assert.Contains(t, names, "uretprobe")
	assert.Contains(t, names, "tracepoint")
	assert.Contains(t, names, "fentry")
	assert.Contains(t, names, "fexit")
	assert.Len(t, names, 10)
}

func TestAllLinkKinds(t *testing.T) {
	kinds := bpfman.AllLinkKinds()

	assert.Contains(t, kinds, bpfman.LinkKindXDP)
	assert.Contains(t, kinds, bpfman.LinkKindTC)
	assert.Contains(t, kinds, bpfman.LinkKindTCX)
	assert.Contains(t, kinds, bpfman.LinkKindKprobe)
	assert.Contains(t, kinds, bpfman.LinkKindKretprobe)
	assert.Contains(t, kinds, bpfman.LinkKindUprobe)
	assert.Contains(t, kinds, bpfman.LinkKindUretprobe)
	assert.Contains(t, kinds, bpfman.LinkKindTracepoint)
	assert.Contains(t, kinds, bpfman.LinkKindFentry)
	assert.Contains(t, kinds, bpfman.LinkKindFexit)
	assert.Len(t, kinds, 10)
}
