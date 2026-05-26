package semantics

import (
	"fmt"
)

// OriginKind identifies what kind of thing a Value represents. It is
// a closed set used for command-parser type checks and for uniform
// error messages. The kind is declared at construction time by the
// code that produces the Value; consumers (command parsers, assert,
// if) check it via ExpectOrigin.
//
// OriginUnknown is the default and acts as a wildcard in ExpectOrigin:
// values with no declared kind (e.g. JSON parsed without explicit
// tagging, map literals, path-lookup results) pass all origin checks.
// This preserves the existing fallback behaviour where the consumer
// tries a structural extraction (capability interface, path lookup)
// regardless of origin.
type OriginKind int

const (
	OriginUnknown OriginKind = iota
	OriginScalar
	OriginBool
	OriginProgram
	OriginLink
	OriginDispatcher
	OriginMap
	// OriginEnvelope tags a Value that wraps a captured-result
	// Envelope: the structured shape every command form returns,
	// carrying ok, code, stdout, stderr, the typed payload value,
	// and an optional pid. Field access walks the standard path
	// machinery; the original Go Envelope is recoverable via
	// Origin() so consumers reach the typed payload Value without
	// a JSON round-trip.
	OriginEnvelope
	// OriginJob tags a Value that wraps a Job: the user-visible
	// handle for a background process started by 'start COMMAND
	// ARGS'. The handle exposes 'pid' through the standard
	// path-walker; the remaining state (captured stdout, stderr,
	// exit code) flows through 'wait' and 'kill', which the
	// driver provides. A job handle is an execution capability,
	// not ordinary immutable data: its internal state evolves as
	// the underlying process runs and exits.
	OriginJob
	// OriginNull is a Value that represents JSON null — a value
	// that is present but whose content is null.  Distinct from
	// an absent (zero) Value: an absent Value is a lookup miss or
	// an uninitialised slot, whereas an OriginNull value is what
	// jq returns when a filter selects a missing field, what
	// commands return when they explicitly produce a null result,
	// and what users can get by asking for one.  The
	// distinction matters at substitution and assignment
	// boundaries: a null is assignable and renderable as "null";
	// an absent value trips "produces no assignable value".
	OriginNull
	// OriginNetPair tags a Value that wraps a NetPair: the
	// handle returned by `net veth-pair`. It owns a single
	// veth pair plus the netns the peer sits in, plus the host
	// and peer addresses, and exposes their names through the
	// standard path-walker so the script can pass
	// $pair.host_link to `bpfman link attach` and $pair.peer_addr
	// to commands like ping. A NetPair handle is a lifecycle
	// capability, not ordinary immutable data: `net release`
	// consumes the topology after which `net exec` / `net start`
	// against the same handle is a runtime error. Field reads
	// remain valid after release because the strings are a
	// historical description of what existed.
	OriginNetPair
)

// String returns the canonical name used in user-facing error
// messages. The names are user-facing labels, not the Go type
// identifiers: OriginEnvelope renders as "result" because that
// is what users call the captured outcome of running a command,
// not the implementation name "envelope".
func (k OriginKind) String() string {
	switch k {
	case OriginUnknown:
		return "unknown"
	case OriginScalar:
		return "scalar"
	case OriginBool:
		return "boolean"
	case OriginProgram:
		return "program"
	case OriginLink:
		return "link"
	case OriginDispatcher:
		return "dispatcher"
	case OriginMap:
		return "map"
	case OriginEnvelope:
		return "result"
	case OriginJob:
		return "job"
	case OriginNull:
		return "null"
	case OriginNetPair:
		return "net pair"
	default:
		return fmt.Sprintf("OriginKind(%d)", int(k))
	}
}

// Shape describes the structural type of a Value at static-check
// time. A Shape is sealed when its field set is exhaustive (a
// record-shaped type with a fixed schema); an unsealed Shape
// permits any field name (a map, an unknown lookup result, or a
// not-yet-registered kind). Scalar leaves are sealed with no
// fields. List shapes carry an Elem describing the element type
// so paths that index into a list can keep walking.
//
// The Shape tree is what the static checker walks when it sees a
// path like $prog.record.program_id: descend Fields["record"],
// then Fields["program_id"], reporting the first segment that
// is not in a sealed parent's field set. Levels reported by an
// unsealed Shape are unconditionally accepted.
type Shape struct {
	// Sealed reports whether Fields enumerates the record's
	// structure exhaustively. False means "any field is
	// allowed"; true means "only entries in Fields are valid",
	// and a missing Fields map (zero map) means "no fields are
	// valid at all" (scalar/bool/null leaves).
	Sealed bool
	// Kind is the OriginKind tag this Shape implies. Used by
	// the checker so a path traversal can produce a leaf kind
	// for downstream inference (let q = $r.code -> q has kind
	// Scalar).
	Kind OriginKind
	// Fields maps a field name to its child Shape. Only
	// consulted when Sealed.
	Fields map[string]Shape
	// Elem describes the shape of a list element when the
	// surrounding Shape is itself a list. Nil for non-list
	// Shapes. The walker descends into Elem when a path
	// segment is "[N]".
	Elem *Shape
}

var (
	// shapeRegistry holds the Shape for each OriginKind. Shell-
	// native kinds are written out directly here; the richer
	// bpfman domain kinds are reflected once in bpfman.go and
	// included as ordinary entries, so the checker and runtime
	// read one declarative table rather than relying on cmd-side
	// init mutation.
	shapeRegistry = map[OriginKind]Shape{
		OriginScalar:  {Sealed: true, Kind: OriginScalar},
		OriginBool:    {Sealed: true, Kind: OriginBool},
		OriginNull:    {Sealed: true, Kind: OriginNull},
		OriginProgram: programShape,
		OriginLink:    linkShape,
		OriginJob: {
			Sealed: true,
			Kind:   OriginJob,
			Fields: map[string]Shape{
				"pid": {Sealed: true, Kind: OriginScalar},
				// target_binary is populated by fire kinds with
				// NeedsBinary == true and by plain start (best-
				// effort identity from argv[0]); the static
				// checker accepts the path here so a script can
				// pass it to bpfman link attach uprobe --target.
				// The runtime ValueFromJob only writes the field
				// when the producer set Job.TargetBinary, so a
				// read on a Job that never carried a target is a
				// runtime field error rather than a silent empty.
				"target_binary": {Sealed: true, Kind: OriginScalar},
			},
		},
		OriginEnvelope: {
			Sealed: true,
			Kind:   OriginEnvelope,
			Fields: map[string]Shape{
				"ok":     {Sealed: true, Kind: OriginBool},
				"code":   {Sealed: true, Kind: OriginScalar},
				"stdout": {Sealed: true, Kind: OriginScalar},
				"stderr": {Sealed: true, Kind: OriginScalar},
				"killed": {Sealed: true, Kind: OriginBool},
				"signal": {Sealed: true, Kind: OriginScalar},
				"pid":    {Sealed: true, Kind: OriginScalar},
			},
		},
		OriginNetPair: {
			Sealed: true,
			Kind:   OriginNetPair,
			Fields: map[string]Shape{
				"ns":           {Sealed: true, Kind: OriginScalar},
				"host_link":    {Sealed: true, Kind: OriginScalar},
				"peer_link":    {Sealed: true, Kind: OriginScalar},
				"host_addr":    {Sealed: true, Kind: OriginScalar},
				"peer_addr":    {Sealed: true, Kind: OriginScalar},
				"host_ifindex": {Sealed: true, Kind: OriginScalar},
				"host_nsid":    {Sealed: true, Kind: OriginScalar},
			},
		},
		// OriginDispatcher is reserved: the kind exists in the
		// enum and renders as "dispatcher" in error messages,
		// but no construction site currently emits a Value
		// tagged with it. The dispatcher subcommands
		// (`bpfman dispatcher list/get/delete`) print
		// formatted output and bind only an envelope through
		// the bind path. When a future refactor returns
		// typed snapshots from `bpfman dispatcher get`,
		// the semantic table grows by one entry alongside
		// Program and Link.
		OriginMap:     {Sealed: false, Kind: OriginMap},
		OriginUnknown: {Sealed: false, Kind: OriginUnknown},
	}
)

// KindShape returns the Shape registered for k. Kinds that are
// not in the registry default to an unsealed Shape carrying that
// kind, which the checker treats as "permit any path" so absence
// of a registration does not produce false positives.
func KindShape(k OriginKind) Shape {
	if s, ok := shapeRegistry[k]; ok {
		return s
	}
	return Shape{Sealed: false, Kind: k}
}

// CloneShape returns a deep copy of s so that mutations to the
// returned value's Fields map or Elem pointer do not leak back
// into any registry entry s may have come from. Caller-side
// composition (e.g. overlaying a discriminated subfield onto a
// generic kind Shape) needs a fresh starting point that aliases
// nothing.
func CloneShape(s Shape) Shape {
	out := Shape{Sealed: s.Sealed, Kind: s.Kind}
	if s.Fields != nil {
		out.Fields = make(map[string]Shape, len(s.Fields))
		for k, v := range s.Fields {
			out.Fields[k] = CloneShape(v)
		}
	}
	if s.Elem != nil {
		e := CloneShape(*s.Elem)
		out.Elem = &e
	}
	return out
}
