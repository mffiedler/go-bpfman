package main

import (
	"fmt"
	"reflect"

	"github.com/alecthomas/kong"
)

// programIDMapper creates a Kong mapper for ProgramID.
func programIDMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("program-id", &s); err != nil {
			return err
		}
		id, err := ParseProgramID(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(id))
		return nil
	}
}

// linkIDMapper creates a Kong mapper for LinkID.
func linkIDMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("link-id", &s); err != nil {
			return err
		}
		id, err := ParseLinkID(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(id))
		return nil
	}
}

// keyValueMapper creates a Kong mapper for KeyValue.
func keyValueMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("key=value", &s); err != nil {
			return err
		}
		kv, err := ParseKeyValue(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(kv))
		return nil
	}
}

// globalDataMapper creates a Kong mapper for GlobalData.
func globalDataMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("name=hex", &s); err != nil {
			return err
		}
		gd, err := ParseGlobalData(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(gd))
		return nil
	}
}

// objectPathMapper creates a Kong mapper for ObjectPath.
func objectPathMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("path", &s); err != nil {
			return err
		}
		op, err := ParseObjectPath(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(op))
		return nil
	}
}

// programSpecMapper creates a Kong mapper for ProgramSpec.
func programSpecMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("type:name", &s); err != nil {
			return err
		}
		ps, err := ParseProgramSpec(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(ps))
		return nil
	}
}

// imagePullPolicyMapper creates a Kong mapper for ImagePullPolicy.
func imagePullPolicyMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("policy", &s); err != nil {
			return err
		}
		pp, err := ParseImagePullPolicy(s)
		if err != nil {
			return err
		}
		target.Set(reflect.ValueOf(pp))
		return nil
	}
}

// outputValueMapper creates a Kong mapper for OutputValue that rejects multiple -o flags.
func outputValueMapper() kong.MapperFunc {
	return func(ctx *kong.DecodeContext, target reflect.Value) error {
		var s string
		if err := ctx.Scan.PopValueInto("format", &s); err != nil {
			return err
		}
		current := target.Interface().(OutputValue)
		if current.IsSet {
			return fmt.Errorf("only one output format may be specified")
		}
		target.Set(reflect.ValueOf(OutputValue{Value: s, IsSet: true}))
		return nil
	}
}
