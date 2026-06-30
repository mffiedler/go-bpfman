// Package args provides the typed argument and flag values the bpfman
// CLI binds to, each paired with a parser that validates its input.
//
// Rather than accept raw strings and validate them deep inside a
// command, the CLI binds each operand or flag to a small wrapper type
// here -- ProgramID, LinkID, KeyValue, GlobalData, ObjectPath -- whose
// parser, called by Kong at bind time, returns
// either a valid value or an error. An ill-formed program ID or
// "KEY=value" pair is therefore rejected before the command runs, and
// the command body receives a value it can trust. This is the
// parse-don't-validate discipline: the wrapper type carries the proof
// of validation in its own structure, so downstream code cannot forget
// to check.
//
// The map helpers (MetadataMap, GlobalDataMap) and the program-type
// parser (ParseProgramTypes) fold the parsed values into the shapes the
// manager API expects.
package args
