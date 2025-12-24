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

package v1alpha1

import (
	"testing"
)

func TestGetImageRegistry(t *testing.T) {
	tests := []struct {
		name     string
		spec     LiqoUpgradeSpec
		expected string
	}{
		{
			name:     "returns default when ImageRegistry is empty",
			spec:     LiqoUpgradeSpec{},
			expected: DefaultImageRegistry,
		},
		{
			name:     "returns default when ImageRegistry is empty string",
			spec:     LiqoUpgradeSpec{ImageRegistry: ""},
			expected: DefaultImageRegistry,
		},
		{
			name:     "returns custom registry when specified",
			spec:     LiqoUpgradeSpec{ImageRegistry: "my-registry.com/liqo"},
			expected: "my-registry.com/liqo",
		},
		{
			name:     "returns custom registry with port",
			spec:     LiqoUpgradeSpec{ImageRegistry: "registry.example.com:5000/liqo"},
			expected: "registry.example.com:5000/liqo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.spec.GetImageRegistry()
			if result != tt.expected {
				t.Errorf("GetImageRegistry() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestDefaultImageRegistryConstant(t *testing.T) {
	// Verify the constant has the expected value
	expected := "ghcr.io/liqotech"
	if DefaultImageRegistry != expected {
		t.Errorf("DefaultImageRegistry = %v, want %v", DefaultImageRegistry, expected)
	}
}
