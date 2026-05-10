package main

import (
	"testing"

	"github.com/frobware/go-bpfman/shell"
)

// litArg fabricates a literal-text Arg so the helpers can be
// driven without going through the parser.
func litArg(s string) shell.Arg {
	return shell.ScalarValueArg{Text: s}
}

func TestU32LE_FormatsLittleEndian(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"0", "00000000"},
		{"1", "01000000"},
		{"255", "ff000000"},
		{"256", "00010000"},
		{"12345", "39300000"}, // 12345 == 0x3039
		{"4294967295", "ffffffff"},
		{"0x3039", "39300000"},
		{"0xdeadbeef", "efbeadde"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			v, err := replU32LE([]shell.Arg{litArg(c.in)})
			if err != nil {
				t.Fatalf("u32le %s: %v", c.in, err)
			}
			got, _ := v.Scalar()
			if got != c.want {
				t.Errorf("u32le %s: got %q want %q", c.in, got, c.want)
			}
		})
	}
}

func TestU64LE_FormatsLittleEndian(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"0", "0000000000000000"},
		{"1", "0100000000000000"},
		{"42", "2a00000000000000"},
		{"18446744073709551615", "ffffffffffffffff"}, // UINT64_MAX
		{"0xdeadbeefcafebabe", "bebafecaefbeadde"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			v, err := replU64LE([]shell.Arg{litArg(c.in)})
			if err != nil {
				t.Fatalf("u64le %s: %v", c.in, err)
			}
			got, _ := v.Scalar()
			if got != c.want {
				t.Errorf("u64le %s: got %q want %q", c.in, got, c.want)
			}
		})
	}
}

func TestU32LE_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []shell.Arg
	}{
		{"no args", nil},
		{"two args", []shell.Arg{litArg("1"), litArg("2")}},
		{"empty", []shell.Arg{litArg("")}},
		{"negative", []shell.Arg{litArg("-1")}},
		{"non-integer", []shell.Arg{litArg("abc")}},
		{"overflow u32", []shell.Arg{litArg("4294967296")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := replU32LE(c.args); err == nil {
				t.Fatalf("u32le %v: expected error, got nil", c.args)
			}
		})
	}
}

func TestU64LE_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []shell.Arg
	}{
		{"no args", nil},
		{"negative", []shell.Arg{litArg("-1")}},
		{"non-integer", []shell.Arg{litArg("xyz")}},
		{"overflow u64", []shell.Arg{litArg("18446744073709551616")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := replU64LE(c.args); err == nil {
				t.Fatalf("u64le %v: expected error, got nil", c.args)
			}
		})
	}
}
