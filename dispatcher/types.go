package dispatcher

// DispatcherType represents the type of dispatcher (XDP or TC).
type DispatcherType string

const (
	DispatcherTypeXDP       DispatcherType = "xdp"
	DispatcherTypeTCIngress DispatcherType = "tc-ingress"
	DispatcherTypeTCEgress  DispatcherType = "tc-egress"
)
