package main

import (
	"reflect"
	"testing"
)

func TestMaybeInjectServe(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"no args",
			[]string{"bpfman"},
			[]string{"bpfman"},
		},
		{
			"csi-support only",
			[]string{"bpfman", "--csi-support"},
			[]string{"bpfman", "serve", "--csi-support"},
		},
		{
			"csi-support followed by another flag",
			[]string{"bpfman", "--csi-support", "--tcp-address=:50051"},
			[]string{"bpfman", "serve", "--csi-support", "--tcp-address=:50051"},
		},
		{
			"csi-support followed by separated flag value",
			[]string{"bpfman", "--csi-support", "--tcp-address", ":50051"},
			[]string{"bpfman", "serve", "--csi-support", "--tcp-address", ":50051"},
		},
		{
			"explicit subcommand",
			[]string{"bpfman", "get", "link", "5"},
			[]string{"bpfman", "get", "link", "5"},
		},
		{
			"version subcommand",
			[]string{"bpfman", "version"},
			[]string{"bpfman", "version"},
		},
		{
			"explicit serve",
			[]string{"bpfman", "serve", "--csi-support"},
			[]string{"bpfman", "serve", "--csi-support"},
		},
		{
			"non-marker flag alone",
			[]string{"bpfman", "--tcp-address=:50051"},
			[]string{"bpfman", "--tcp-address=:50051"},
		},
		{
			"marker not at argv[1]",
			[]string{"bpfman", "--tcp-address=:50051", "--csi-support"},
			[]string{"bpfman", "--tcp-address=:50051", "--csi-support"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := maybeInjectServe(tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("maybeInjectServe(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
