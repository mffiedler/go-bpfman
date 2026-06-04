package verify

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sigstore/cosign/v2/pkg/cosign"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/platform"
)

func TestNoSignReportsVerificationDisabled(t *testing.T) {
	t.Parallel()

	result, err := NoSign().Verify(context.Background(), "example.test/x:latest")
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if result.Status != platform.SignatureVerificationDisabled {
		t.Fatalf("Status = %q, want %q", result.Status, platform.SignatureVerificationDisabled)
	}
}

func TestIsNoSignaturesError(t *testing.T) {
	t.Parallel()

	if !isNoSignaturesError(fmt.Errorf("wrapped: %w", &cosign.ErrNoSignaturesFound{})) {
		t.Fatal("isNoSignaturesError returned false for ErrNoSignaturesFound")
	}
}

func TestIsNoSignaturesErrorRejectsNoMatchingSignatures(t *testing.T) {
	t.Parallel()

	if isNoSignaturesError(fmt.Errorf("wrapped: %w", &cosign.ErrNoMatchingSignatures{})) {
		t.Fatal("isNoSignaturesError returned true for ErrNoMatchingSignatures")
	}
}

func TestFromSigningConfigReturnsNoSignWhenVerificationDisabled(t *testing.T) {
	t.Parallel()

	cfg := config.SigningConfig{
		AllowUnsigned: true,
		VerifyEnabled: false,
	}
	verifier, err := FromSigningConfig(cfg, nil)
	if err != nil {
		t.Fatalf("FromSigningConfig returned error: %v", err)
	}
	result, err := verifier.Verify(context.Background(), "example.test/x:latest")
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if result.Status != platform.SignatureVerificationDisabled {
		t.Fatalf("Status = %q, want %q", result.Status, platform.SignatureVerificationDisabled)
	}
}

func TestFromSigningConfigDefaultsToAnyIdentityWhenUnsignedForbidden(t *testing.T) {
	t.Parallel()

	cfg := config.SigningConfig{
		AllowUnsigned: false,
		VerifyEnabled: true,
	}
	verifier, err := FromSigningConfig(cfg, nil)
	if err != nil {
		t.Fatalf("FromSigningConfig returned error: %v", err)
	}
	cosignVerifier, ok := verifier.(*cosignVerifier)
	if !ok {
		t.Fatalf("verifier has type %T, want *cosignVerifier", verifier)
	}
	if cosignVerifier.allowUnsigned {
		t.Fatal("allowUnsigned = true, want false")
	}
	if len(cosignVerifier.identities) != 1 {
		t.Fatalf("len(identities) = %d, want 1", len(cosignVerifier.identities))
	}
	if cosignVerifier.identities[0].SubjectRegExp != ".*" {
		t.Fatalf("SubjectRegExp = %q, want .*", cosignVerifier.identities[0].SubjectRegExp)
	}
	if cosignVerifier.identities[0].IssuerRegExp != ".*" {
		t.Fatalf("IssuerRegExp = %q, want .*", cosignVerifier.identities[0].IssuerRegExp)
	}
}

func TestFromSigningConfigWarnsWhenUnsignedForbiddenWithoutTrustedIdentity(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.SigningConfig{
		AllowUnsigned: false,
		VerifyEnabled: true,
	}
	if _, err := FromSigningConfig(cfg, logger); err != nil {
		t.Fatalf("FromSigningConfig returned error: %v", err)
	}
	if !strings.Contains(logs.String(), "accepting any valid signer") {
		t.Fatalf("logs = %q, want any-signer warning", logs.String())
	}
}

func TestFromSigningConfigAppliesTrustedIdentity(t *testing.T) {
	t.Parallel()

	cfg := config.SigningConfig{
		AllowUnsigned: false,
		VerifyEnabled: true,
		TrustedIdentities: []config.TrustedIdentityConfig{
			{
				CertificateIdentityRegexp:   `.*@example\.com`,
				CertificateOIDCIssuerRegexp: `https://github\.com/.*`,
			},
			{
				CertificateIdentity:   "builder@example.com",
				CertificateOIDCIssuer: "https://accounts.google.com",
			},
		},
	}
	verifier, err := FromSigningConfig(cfg, nil)
	if err != nil {
		t.Fatalf("FromSigningConfig returned error: %v", err)
	}
	cosignVerifier, ok := verifier.(*cosignVerifier)
	if !ok {
		t.Fatalf("verifier has type %T, want *cosignVerifier", verifier)
	}
	if cosignVerifier.allowUnsigned {
		t.Fatal("allowUnsigned = true, want false")
	}
	if len(cosignVerifier.identities) != 2 {
		t.Fatalf("len(identities) = %d, want 2", len(cosignVerifier.identities))
	}
	first := cosignVerifier.identities[0]
	wantSubjectRegexp := `^(?:.*@example\.com)$`
	if first.SubjectRegExp != wantSubjectRegexp {
		t.Fatalf("SubjectRegExp = %q, want %q", first.SubjectRegExp, wantSubjectRegexp)
	}
	wantIssuerRegexp := `^(?:https://github\.com/.*)$`
	if first.IssuerRegExp != wantIssuerRegexp {
		t.Fatalf("IssuerRegExp = %q, want %q", first.IssuerRegExp, wantIssuerRegexp)
	}
	second := cosignVerifier.identities[1]
	if second.Subject != "builder@example.com" {
		t.Fatalf("Subject = %q, want builder@example.com", second.Subject)
	}
	if second.Issuer != "https://accounts.google.com" {
		t.Fatalf("Issuer = %q, want https://accounts.google.com", second.Issuer)
	}
	if second.SubjectRegExp != "" {
		t.Fatalf("SubjectRegExp = %q, want empty", second.SubjectRegExp)
	}
	if second.IssuerRegExp != "" {
		t.Fatalf("IssuerRegExp = %q, want empty", second.IssuerRegExp)
	}
}
