// Package verify provides OCI image signature verification.
package verify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/fulcio"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/options"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/rekor"
	"github.com/sigstore/cosign/v2/pkg/cosign"

	"github.com/frobware/go-bpfman/platform"
)

// NoSign returns a verifier that always succeeds without checking signatures.
// Use this when signature verification is disabled.
func NoSign() platform.SignatureVerifier {
	return noSignVerifier{}
}

type noSignVerifier struct{}

func (noSignVerifier) Verify(ctx context.Context, imageRef string) error {
	return nil
}

// CosignOption configures a cosign verifier.
type CosignOption func(*cosignVerifier)

// WithLogger sets the logger for verification operations.
func WithLogger(logger *slog.Logger) CosignOption {
	return func(v *cosignVerifier) {
		v.logger = logger
	}
}

// WithAllowUnsigned controls whether unsigned images are accepted.
func WithAllowUnsigned(allow bool) CosignOption {
	return func(v *cosignVerifier) {
		v.allowUnsigned = allow
	}
}

// WithIdentity sets the certificate identity constraints.
// Use ".*" for either value to accept any valid certificate.
func WithIdentity(issuerRegexp, subjectRegexp string) CosignOption {
	return func(v *cosignVerifier) {
		v.issuerRegexp = issuerRegexp
		v.subjectRegexp = subjectRegexp
	}
}

// Cosign returns a verifier that uses sigstore/cosign for signature verification.
func Cosign(opts ...CosignOption) platform.SignatureVerifier {
	v := &cosignVerifier{
		logger:        slog.Default(),
		allowUnsigned: true, // Permissive default
		issuerRegexp:  ".*", // Accept any issuer by default
		subjectRegexp: ".*", // Accept any subject by default
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// cosignVerifier verifies OCI image signatures using cosign/sigstore.
type cosignVerifier struct {
	logger        *slog.Logger
	allowUnsigned bool
	issuerRegexp  string
	subjectRegexp string
}

// Verify checks that the image has a valid sigstore signature.
func (v *cosignVerifier) Verify(ctx context.Context, imageRef string) error {
	logger := v.logger.With("image", imageRef)
	logger.Debug("verifying image signature")

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("failed to parse image reference: %w", err)
	}

	rootCerts, err := fulcio.GetRoots()
	if err != nil {
		return fmt.Errorf("failed to get Fulcio root certificates: %w", err)
	}

	intermediateCerts, err := fulcio.GetIntermediates()
	if err != nil {
		return fmt.Errorf("failed to get Fulcio intermediate certificates: %w", err)
	}

	rekorClient, err := rekor.NewClient(options.DefaultRekorURL)
	if err != nil {
		return fmt.Errorf("failed to create Rekor client: %w", err)
	}

	rekorPubKeys, err := cosign.GetRekorPubs(ctx)
	if err != nil {
		return fmt.Errorf("failed to get Rekor public keys: %w", err)
	}

	ctLogPubKeys, err := cosign.GetCTLogPubs(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CT log public keys: %w", err)
	}

	co := &cosign.CheckOpts{
		RekorClient:       rekorClient,
		RekorPubKeys:      rekorPubKeys,
		RootCerts:         rootCerts,
		IntermediateCerts: intermediateCerts,
		CTLogPubKeys:      ctLogPubKeys,
		Identities: []cosign.Identity{
			{
				IssuerRegExp:  v.issuerRegexp,
				SubjectRegExp: v.subjectRegexp,
			},
		},
	}

	logger.Debug("calling cosign.VerifyImageSignatures",
		"issuer_regexp", v.issuerRegexp,
		"subject_regexp", v.subjectRegexp,
	)
	signatures, bundleVerified, err := cosign.VerifyImageSignatures(ctx, ref, co)
	if err != nil {
		logger.Debug("VerifyImageSignatures returned error", "error", err)
		if isNoSignaturesError(err) {
			if v.allowUnsigned {
				logger.Debug("image has no signatures, but unsigned images are allowed")
				return nil
			}
			logger.Error("image has no signatures and unsigned images are not allowed")
			return fmt.Errorf("image %s has no signatures and unsigned images are not allowed", imageRef)
		}
		logger.Error("signature verification failed", "error", err)
		return fmt.Errorf("signature verification failed for %s: %w", imageRef, err)
	}

	logger.Info("image signature verified",
		"signatures", len(signatures),
		"bundle_verified", bundleVerified,
	)

	return nil
}

func isNoSignaturesError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "no matching signatures") ||
		strings.Contains(errMsg, "no signatures found") ||
		strings.Contains(errMsg, "MANIFEST_UNKNOWN")
}
