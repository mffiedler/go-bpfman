package bpfman_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

func TestHasActiveLinks(t *testing.T) {
	tests := []struct {
		name     string
		prog     *bpfman.Program
		expected bool
	}{
		{
			name: "no links",
			prog: &bpfman.Program{
				Status: bpfman.ProgramStatus{
					Links: nil,
				},
			},
			expected: false,
		},
		{
			name: "empty links slice",
			prog: &bpfman.Program{
				Status: bpfman.ProgramStatus{
					Links: []bpfman.Link{},
				},
			},
			expected: false,
		},
		{
			name: "link with nil kernel (stale DB record)",
			prog: &bpfman.Program{
				Status: bpfman.ProgramStatus{
					Links: []bpfman.Link{
						{Status: bpfman.LinkStatus{Kernel: nil}},
					},
				},
			},
			expected: false,
		},
		{
			name: "link with kernel presence",
			prog: &bpfman.Program{
				Status: bpfman.ProgramStatus{
					Links: []bpfman.Link{
						{Status: bpfman.LinkStatus{Kernel: &kernel.Link{ID: 1}}},
					},
				},
			},
			expected: true,
		},
		{
			name: "mixed links - one with kernel, one without",
			prog: &bpfman.Program{
				Status: bpfman.ProgramStatus{
					Links: []bpfman.Link{
						{Status: bpfman.LinkStatus{Kernel: nil}},
						{Status: bpfman.LinkStatus{Kernel: &kernel.Link{ID: 2}}},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &bpfman.ProgramFilter{
				AttachmentState: bpfman.AttachmentStateAttached,
				LabelSelector:   labels.Everything(),
			}
			// Attached filter matches only if hasActiveLinks is true
			assert.Equal(t, tt.expected, filter.Matches(tt.prog))
		})
	}
}

func TestAttachmentStateFilter(t *testing.T) {
	progWithLinks := &bpfman.Program{
		Status: bpfman.ProgramStatus{
			Links: []bpfman.Link{
				{Status: bpfman.LinkStatus{Kernel: &kernel.Link{ID: 1}}},
			},
		},
	}
	progWithoutLinks := &bpfman.Program{
		Status: bpfman.ProgramStatus{
			Links: nil,
		},
	}

	tests := []struct {
		name            string
		attachmentState bpfman.AttachmentState
		prog            *bpfman.Program
		expected        bool
	}{
		{"all matches attached", bpfman.AttachmentStateAll, progWithLinks, true},
		{"all matches unattached", bpfman.AttachmentStateAll, progWithoutLinks, true},
		{"attached matches attached", bpfman.AttachmentStateAttached, progWithLinks, true},
		{"attached rejects unattached", bpfman.AttachmentStateAttached, progWithoutLinks, false},
		{"unattached rejects attached", bpfman.AttachmentStateUnattached, progWithLinks, false},
		{"unattached matches unattached", bpfman.AttachmentStateUnattached, progWithoutLinks, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &bpfman.ProgramFilter{
				AttachmentState: tt.attachmentState,
				LabelSelector:   labels.Everything(),
			}
			assert.Equal(t, tt.expected, filter.Matches(tt.prog))
		})
	}
}

func TestTypeFilter(t *testing.T) {
	xdpProg := &bpfman.Program{
		Spec: bpfman.ProgramSpec{
			Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeXDP},
		},
	}
	kprobeProg := &bpfman.Program{
		Spec: bpfman.ProgramSpec{
			Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeKprobe},
		},
	}

	tests := []struct {
		name     string
		types    map[bpfman.ProgramType]struct{}
		prog     *bpfman.Program
		expected bool
	}{
		{
			name:     "empty type set matches all",
			types:    nil,
			prog:     xdpProg,
			expected: true,
		},
		{
			name:     "single type matches",
			types:    map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}},
			prog:     xdpProg,
			expected: true,
		},
		{
			name:     "single type rejects non-match",
			types:    map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}},
			prog:     kprobeProg,
			expected: false,
		},
		{
			name: "multiple types - matches first",
			types: map[bpfman.ProgramType]struct{}{
				bpfman.ProgramTypeXDP:    {},
				bpfman.ProgramTypeKprobe: {},
			},
			prog:     xdpProg,
			expected: true,
		},
		{
			name: "multiple types - matches second",
			types: map[bpfman.ProgramType]struct{}{
				bpfman.ProgramTypeXDP:    {},
				bpfman.ProgramTypeKprobe: {},
			},
			prog:     kprobeProg,
			expected: true,
		},
		{
			name: "multiple types - rejects non-match",
			types: map[bpfman.ProgramType]struct{}{
				bpfman.ProgramTypeXDP:    {},
				bpfman.ProgramTypeKprobe: {},
			},
			prog: &bpfman.Program{
				Spec: bpfman.ProgramSpec{
					Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeFentry},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &bpfman.ProgramFilter{
				Types:         tt.types,
				LabelSelector: labels.Everything(),
			}
			assert.Equal(t, tt.expected, filter.Matches(tt.prog))
		})
	}
}

func TestLabelSelectorFilter(t *testing.T) {
	tests := []struct {
		name           string
		selector       string
		metadata       map[string]string
		expectedMatch  bool
		expectParseErr bool
	}{
		{
			name:          "empty selector matches all",
			selector:      "",
			metadata:      map[string]string{"app": "test"},
			expectedMatch: true,
		},
		{
			name:          "equality selector matches",
			selector:      "app=test",
			metadata:      map[string]string{"app": "test"},
			expectedMatch: true,
		},
		{
			name:          "equality selector rejects",
			selector:      "app=test",
			metadata:      map[string]string{"app": "other"},
			expectedMatch: false,
		},
		{
			name:          "inequality selector matches",
			selector:      "app!=test",
			metadata:      map[string]string{"app": "other"},
			expectedMatch: true,
		},
		{
			name:          "multiple selectors - all match",
			selector:      "app=test,version=v1",
			metadata:      map[string]string{"app": "test", "version": "v1"},
			expectedMatch: true,
		},
		{
			name:          "multiple selectors - partial match fails",
			selector:      "app=test,version=v1",
			metadata:      map[string]string{"app": "test", "version": "v2"},
			expectedMatch: false,
		},
		{
			name:          "in selector matches",
			selector:      "app in (foo,bar)",
			metadata:      map[string]string{"app": "bar"},
			expectedMatch: true,
		},
		{
			name:          "notin selector matches",
			selector:      "app notin (foo,bar)",
			metadata:      map[string]string{"app": "baz"},
			expectedMatch: true,
		},
		{
			name:          "exists selector matches",
			selector:      "app",
			metadata:      map[string]string{"app": "test"},
			expectedMatch: true,
		},
		{
			name:          "exists selector rejects missing key",
			selector:      "app",
			metadata:      map[string]string{"other": "test"},
			expectedMatch: false,
		},
		{
			name:          "not exists selector matches missing key",
			selector:      "!debug",
			metadata:      map[string]string{"app": "test"},
			expectedMatch: true,
		},
		{
			name:          "not exists selector rejects present key",
			selector:      "!debug",
			metadata:      map[string]string{"debug": "true"},
			expectedMatch: false,
		},
		{
			name:          "nil metadata with key requirement fails",
			selector:      "app=test",
			metadata:      nil,
			expectedMatch: false,
		},
		{
			name:          "nil metadata with negation passes",
			selector:      "!debug",
			metadata:      nil,
			expectedMatch: true,
		},
		{
			name:           "invalid selector syntax",
			selector:       "===invalid",
			metadata:       nil,
			expectParseErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sel labels.Selector
			if tt.selector == "" {
				sel = labels.Everything()
			} else {
				var err error
				sel, err = labels.Parse(tt.selector)
				if tt.expectParseErr {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
			}

			filter := &bpfman.ProgramFilter{
				LabelSelector: sel,
			}
			prog := &bpfman.Program{
				Spec: bpfman.ProgramSpec{
					Meta: bpfman.ProgramMeta{Metadata: tt.metadata},
				},
			}
			assert.Equal(t, tt.expectedMatch, filter.Matches(prog))
		})
	}
}

func TestLabelSelectorParity(t *testing.T) {
	// Verify that our filter produces the same result as direct labels.Selector usage
	testCases := []struct {
		selector string
		metadata map[string]string
	}{
		{"app=test", map[string]string{"app": "test"}},
		{"app=test", map[string]string{"app": "other"}},
		{"app in (foo,bar,baz)", map[string]string{"app": "bar"}},
		{"app notin (foo,bar)", map[string]string{"app": "baz"}},
		{"!debug", map[string]string{"app": "test"}},
		{"debug", map[string]string{"debug": "true"}},
		{"app=test,version=v1", map[string]string{"app": "test", "version": "v1", "extra": "ignored"}},
	}

	for _, tc := range testCases {
		t.Run(tc.selector, func(t *testing.T) {
			sel, err := labels.Parse(tc.selector)
			require.NoError(t, err)

			// Direct selector check
			directResult := sel.Matches(labels.Set(tc.metadata))

			// Via filter
			filter := &bpfman.ProgramFilter{LabelSelector: sel}
			prog := &bpfman.Program{
				Spec: bpfman.ProgramSpec{
					Meta: bpfman.ProgramMeta{Metadata: tc.metadata},
				},
			}
			filterResult := filter.Matches(prog)

			assert.Equal(t, directResult, filterResult, "filter should match direct selector result")
		})
	}
}

func TestCombinedFilters(t *testing.T) {
	// Program that matches all criteria
	matchingProg := &bpfman.Program{
		Spec: bpfman.ProgramSpec{
			Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeXDP},
			Meta: bpfman.ProgramMeta{Metadata: map[string]string{"app": "test"}},
		},
		Status: bpfman.ProgramStatus{
			Links: []bpfman.Link{
				{Status: bpfman.LinkStatus{Kernel: &kernel.Link{ID: 1}}},
			},
		},
	}

	sel, err := labels.Parse("app=test")
	require.NoError(t, err)

	filter := &bpfman.ProgramFilter{
		AttachmentState: bpfman.AttachmentStateAttached,
		Types:           map[bpfman.ProgramType]struct{}{bpfman.ProgramTypeXDP: {}},
		LabelSelector:   sel,
	}

	t.Run("all criteria match", func(t *testing.T) {
		assert.True(t, filter.Matches(matchingProg))
	})

	t.Run("wrong type fails", func(t *testing.T) {
		prog := &bpfman.Program{
			Spec: bpfman.ProgramSpec{
				Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeKprobe},
				Meta: bpfman.ProgramMeta{Metadata: map[string]string{"app": "test"}},
			},
			Status: bpfman.ProgramStatus{
				Links: []bpfman.Link{
					{Status: bpfman.LinkStatus{Kernel: &kernel.Link{ID: 1}}},
				},
			},
		}
		assert.False(t, filter.Matches(prog))
	})

	t.Run("wrong labels fails", func(t *testing.T) {
		prog := &bpfman.Program{
			Spec: bpfman.ProgramSpec{
				Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeXDP},
				Meta: bpfman.ProgramMeta{Metadata: map[string]string{"app": "other"}},
			},
			Status: bpfman.ProgramStatus{
				Links: []bpfman.Link{
					{Status: bpfman.LinkStatus{Kernel: &kernel.Link{ID: 1}}},
				},
			},
		}
		assert.False(t, filter.Matches(prog))
	})

	t.Run("not attached fails", func(t *testing.T) {
		prog := &bpfman.Program{
			Spec: bpfman.ProgramSpec{
				Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeXDP},
				Meta: bpfman.ProgramMeta{Metadata: map[string]string{"app": "test"}},
			},
			Status: bpfman.ProgramStatus{
				Links: nil,
			},
		}
		assert.False(t, filter.Matches(prog))
	})
}

func TestNilFilter(t *testing.T) {
	prog := &bpfman.Program{
		Spec: bpfman.ProgramSpec{
			Load: bpfman.ProgramLoadSpec{ProgramType: bpfman.ProgramTypeXDP},
		},
	}

	var filter *bpfman.ProgramFilter
	assert.True(t, filter.Matches(prog), "nil filter should match all programs")
}

func TestProgramTypeConsistency(t *testing.T) {
	// Verify AllProgramTypes and ProgramTypeNames are consistent
	allTypes := bpfman.AllProgramTypes()
	allNames := bpfman.ProgramTypeNames()

	require.Equal(t, len(allTypes), len(allNames), "AllProgramTypes and ProgramTypeNames should have same length")

	for i, pt := range allTypes {
		assert.Equal(t, pt.String(), allNames[i], "ProgramTypeNames[%d] should match AllProgramTypes[%d].String()", i, i)
	}

	// Verify ParseProgramType accepts all names from ProgramTypeNames
	for _, name := range allNames {
		pt, ok := bpfman.ParseProgramType(name)
		assert.True(t, ok, "ParseProgramType should accept %q", name)
		assert.Equal(t, name, pt.String(), "round-trip should preserve name")
	}

	// Verify AllProgramTypes doesn't include Unspecified
	for _, pt := range allTypes {
		assert.NotEqual(t, bpfman.ProgramTypeUnspecified, pt, "AllProgramTypes should not include Unspecified")
	}
}
