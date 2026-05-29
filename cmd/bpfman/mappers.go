package main

import (
	"fmt"
	"reflect"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
)

// programIDMapper creates a Kong mapper for bpfmancli.ProgramID.
func programIDMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("program-id", &s); err != nil {
			return err
		}
		id, err := bpfmancli.ParseProgramID(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(id))
		return nil
	}
}

// linkIDMapper creates a Kong mapper for bpfmancli.LinkID.
func linkIDMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("link-id", &s); err != nil {
			return err
		}
		id, err := bpfmancli.ParseLinkID(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(id))
		return nil
	}
}

// keyValueMapper creates a Kong mapper for bpfmancli.KeyValue.
func keyValueMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("key=value", &s); err != nil {
			return err
		}
		kv, err := bpfmancli.ParseKeyValue(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(kv))
		return nil
	}
}

// globalDataMapper creates a Kong mapper for bpfmancli.GlobalData.
func globalDataMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("name=hex", &s); err != nil {
			return err
		}
		gd, err := bpfmancli.ParseGlobalData(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(gd))
		return nil
	}
}

// objectPathMapper creates a Kong mapper for bpfmancli.ObjectPath.
func objectPathMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("path", &s); err != nil {
			return err
		}
		op, err := bpfmancli.ParseObjectPath(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(op))
		return nil
	}
}

// programSpecMapper creates a Kong mapper for bpfmancli.ProgramSpec.
func programSpecMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("type:name", &s); err != nil {
			return err
		}
		ps, err := bpfmancli.ParseProgramSpec(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(ps))
		return nil
	}
}

// tracepointMapper creates a Kong mapper for bpfman.Tracepoint.
func tracepointMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("group/name", &s); err != nil {
			return err
		}
		tp, err := bpfman.ParseTracepoint(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(tp))
		return nil
	}
}

// imagePullPolicyMapper creates a Kong mapper for bpfmancli.ImagePullPolicy.
func imagePullPolicyMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("policy", &s); err != nil {
			return err
		}
		pp, err := bpfmancli.ParseImagePullPolicy(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(pp))
		return nil
	}
}

// outputValueMapper creates a Kong mapper for cliformat.OutputValue that rejects multiple -o flags.
func outputValueMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("format", &s); err != nil {
			return err
		}
		current := target.Interface().(cliformat.OutputValue)
		if current.IsSet {
			return fmt.Errorf("only one output format may be specified")
		}
		target.Set(reflect.ValueOf(cliformat.OutputValue{Value: s, IsSet: true}))
		return nil
	}
}
