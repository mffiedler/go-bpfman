package bpfman

import "fmt"

// ImagePullPolicy specifies when to pull an OCI image.
type ImagePullPolicy int

const (
	// PullAlways always pulls the image, even if cached.
	PullAlways ImagePullPolicy = iota
	// PullIfNotPresent uses the cache if available, otherwise pulls.
	PullIfNotPresent
	// PullNever only uses the cache, fails if not present.
	PullNever
)

// String returns the string representation of the pull policy.
func (p ImagePullPolicy) String() string {
	switch p {
	case PullAlways:
		return "Always"
	case PullIfNotPresent:
		return "IfNotPresent"
	case PullNever:
		return "Never"
	default:
		return "Unknown"
	}
}

// MarshalText implements encoding.TextMarshaler.
func (p ImagePullPolicy) MarshalText() ([]byte, error) {
	return []byte(p.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *ImagePullPolicy) UnmarshalText(text []byte) error {
	parsed, ok := ParseImagePullPolicy(string(text))
	if !ok {
		return fmt.Errorf("unknown pull policy: %s", text)
	}
	*p = parsed
	return nil
}

// ParseImagePullPolicy parses a string into an ImagePullPolicy.
// Returns the policy and true if valid, or PullIfNotPresent and false if not.
func ParseImagePullPolicy(s string) (ImagePullPolicy, bool) {
	switch s {
	case "Always", "always":
		return PullAlways, true
	case "IfNotPresent", "ifnotpresent":
		return PullIfNotPresent, true
	case "Never", "never":
		return PullNever, true
	default:
		return PullIfNotPresent, false
	}
}
