/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"
)

func TestCompareVersions(t *testing.T) {
	r := &LiqoUpgradeReconciler{}

	tests := []struct {
		name     string
		v1       string
		v2       string
		expected int
	}{
		// Equal versions
		{
			name:     "equal versions - simple",
			v1:       "1.0.0",
			v2:       "1.0.0",
			expected: 0,
		},
		{
			name:     "equal versions - with v prefix",
			v1:       "v1.0.0",
			v2:       "v1.0.0",
			expected: 0,
		},
		// v1 < v2
		{
			name:     "v1 less than v2 - patch",
			v1:       "1.0.0",
			v2:       "1.0.1",
			expected: -1,
		},
		{
			name:     "v1 less than v2 - minor",
			v1:       "1.0.0",
			v2:       "1.1.0",
			expected: -1,
		},
		{
			name:     "v1 less than v2 - major",
			v1:       "1.0.0",
			v2:       "2.0.0",
			expected: -1,
		},
		// v1 > v2
		{
			name:     "v1 greater than v2 - patch",
			v1:       "1.0.1",
			v2:       "1.0.0",
			expected: 1,
		},
		{
			name:     "v1 greater than v2 - minor",
			v1:       "1.1.0",
			v2:       "1.0.0",
			expected: 1,
		},
		{
			name:     "v1 greater than v2 - major",
			v1:       "2.0.0",
			v2:       "1.0.0",
			expected: 1,
		},
		// Edge cases
		{
			name:     "different length versions - v1 shorter",
			v1:       "1.0",
			v2:       "1.0.1",
			expected: -1,
		},
		{
			name:     "different length versions - v2 shorter",
			v1:       "1.0.1",
			v2:       "1.0",
			expected: 1,
		},
		{
			name:     "with v prefix stripped",
			v1:       "v1.0.0",
			v2:       "v1.0.1",
			expected: -1,
		},
		// Real Liqo version examples
		{
			name:     "liqo upgrade v1.0.0 to v1.0.1",
			v1:       "v1.0.0",
			v2:       "v1.0.1",
			expected: -1,
		},
		{
			name:     "liqo downgrade v1.0.1 to v1.0.0",
			v1:       "v1.0.1",
			v2:       "v1.0.0",
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.compareVersions(tt.v1, tt.v2)
			if result != tt.expected {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.v1, tt.v2, result, tt.expected)
			}
		})
	}
}
