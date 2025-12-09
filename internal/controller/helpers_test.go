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

func TestInt32Ptr(t *testing.T) {
	tests := []struct {
		name     string
		input    int32
		expected int32
	}{
		{
			name:     "positive value",
			input:    42,
			expected: 42,
		},
		{
			name:     "zero value",
			input:    0,
			expected: 0,
		},
		{
			name:     "negative value",
			input:    -10,
			expected: -10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := int32Ptr(tt.input)
			if result == nil {
				t.Fatal("int32Ptr returned nil")
			}
			if *result != tt.expected {
				t.Errorf("int32Ptr() = %d, want %d", *result, tt.expected)
			}
		})
	}
}
