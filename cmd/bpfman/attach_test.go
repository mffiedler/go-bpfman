package main

import (
	"context"
	"strings"
	"testing"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

// TestAttachMetadataRecognisedButRejected checks that -m/--metadata is a
// recognised flag on every attach command but is rejected before any
// attach happens, since persisting link metadata is not implemented yet.
// The guard runs before runAttach creates a manager, so a nil CLI is
// never dereferenced.
func TestAttachMetadataRecognisedButRejected(t *testing.T) {
	md := AttachMetadataFlags{Metadata: []bpfmancli.KeyValue{{Key: "owner", Value: "acme"}}}

	type attachCmd interface {
		Run(*bpfmancli.CLI, context.Context) error
	}

	cmds := map[string]attachCmd{
		"xdp":        &AttachXDPCmd{AttachMetadataFlags: md},
		"tc":         &AttachTCCmd{AttachMetadataFlags: md},
		"tcx":        &AttachTCXCmd{AttachMetadataFlags: md},
		"tracepoint": &AttachTracepointCmd{AttachMetadataFlags: md},
		"kprobe":     &AttachKprobeCmd{AttachMetadataFlags: md},
		"uprobe":     &AttachUprobeCmd{AttachMetadataFlags: md},
		"fentry":     &AttachFentryCmd{AttachMetadataFlags: md},
		"fexit":      &AttachFexitCmd{AttachMetadataFlags: md},
	}

	for name, c := range cmds {
		t.Run(name, func(t *testing.T) {
			err := c.Run(nil, context.Background())
			if err == nil {
				t.Fatal("expected metadata to be rejected, got nil error")
			}
			if !strings.Contains(err.Error(), "link metadata is not implemented yet") {
				t.Fatalf("expected %q, got %q", "link metadata is not implemented yet", err.Error())
			}
		})
	}
}
