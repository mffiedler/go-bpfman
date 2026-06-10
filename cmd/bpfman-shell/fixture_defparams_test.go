package main

import "testing"

// TestScriptRun_DefParamKindsFixtureMatrix pins the def call
// boundary's typing rule: variables keep their value kinds across
// the call (a number-valued $n arrives as a number, through direct
// calls, def-to-def calls, field projections, and defer capture),
// while bare literals remain words by shell convention and need the
// jq tonumber idiom for numeric comparison. The
// literal-number-compare-errors fixture pins the asymmetry's error
// shape so the diagnostic stays helpful.
func TestScriptRun_DefParamKindsFixtureMatrix(t *testing.T) {
	t.Parallel()

	cases := []fixtureWorkflowCase{
		{
			name:    "variable_scalar_preserves_kind",
			fixture: "defparams/variable-scalar-preserves-kind",
		},
		{
			name:    "literal_stays_word",
			fixture: "defparams/literal-stays-word",
		},
		{
			name:    "literal_number_compare_errors",
			fixture: "defparams/literal-number-compare-errors",
		},
		{
			name:    "field_projection_preserves_kind",
			fixture: "defparams/field-projection-preserves-kind",
		},
		{
			name:    "transitive_def_call",
			fixture: "defparams/transitive-def-call",
		},
		{
			name:    "defer_captured_variable_keeps_kind",
			fixture: "defparams/defer-captured-variable-keeps-kind",
		},
		{
			name:    "tonumber_identity_both_kinds",
			fixture: "defparams/tonumber-identity-both-kinds",
		},
		{
			name:    "interpolation_of_number_param",
			fixture: "defparams/interpolation-of-number-param",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFixtureWorkflowMatrix(t, tc)
		})
	}
}
