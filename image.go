package bpfman

import "fmt"

// ImagePullPolicy specifies when to pull an OCI image.
// It is an opaque value; the only valid instances are the
// package-level variables or ParseImagePullPolicy.
type ImagePullPolicy struct{ v string }

var (
	// PullAlways always pulls the image, even if cached.
	PullAlways = ImagePullPolicy{"Always"}
	// PullIfNotPresent uses the cache if available, otherwise pulls.
	PullIfNotPresent = ImagePullPolicy{"IfNotPresent"}
	// PullNever only uses the cache, fails if not present.
	PullNever = ImagePullPolicy{"Never"}
)

// Valid reports whether p is a recognised pull policy (not the zero value).
func (p ImagePullPolicy) Valid() bool { return p != (ImagePullPolicy{}) }

// String returns the string representation of the pull policy.
func (p ImagePullPolicy) String() string               { return p.v }
func (p ImagePullPolicy) MarshalText() ([]byte, error) { return []byte(p.v), nil }

func (p *ImagePullPolicy) UnmarshalText(b []byte) error {
	parsed, err := ParseImagePullPolicy(string(b))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// ParseImagePullPolicy parses a string into an ImagePullPolicy.
// Returns the ImagePullPolicy and a nil error if valid, or the zero
// value and an error if not recognised. Matching is case-insensitive.
func ParseImagePullPolicy(s string) (ImagePullPolicy, error) {
	switch s {
	case "Always", "always":
		return PullAlways, nil
	case "IfNotPresent", "ifnotpresent":
		return PullIfNotPresent, nil
	case "Never", "never":
		return PullNever, nil
	default:
		return ImagePullPolicy{}, fmt.Errorf("unknown pull policy %q", s)
	}
}
