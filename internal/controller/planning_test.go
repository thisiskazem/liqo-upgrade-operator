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
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestCompareEnvVars(t *testing.T) {
	r := &LiqoUpgradeReconciler{}

	tests := []struct {
		name          string
		currentEnv    []corev1.EnvVar
		targetEnv     []TargetEnvVar
		expectedCount int
		expectedTypes []string // "add", "update", "remove"
	}{
		{
			name:          "no changes when both empty",
			currentEnv:    []corev1.EnvVar{},
			targetEnv:     []TargetEnvVar{},
			expectedCount: 0,
			expectedTypes: []string{},
		},
		{
			name:       "detect new env var",
			currentEnv: []corev1.EnvVar{},
			targetEnv: []TargetEnvVar{
				{Name: "NEW_VAR", Type: "value", Value: "new-value"},
			},
			expectedCount: 1,
			expectedTypes: []string{"add"},
		},
		{
			name: "detect removed env var",
			currentEnv: []corev1.EnvVar{
				{Name: "OLD_VAR", Value: "old-value"},
			},
			targetEnv:     []TargetEnvVar{},
			expectedCount: 1,
			expectedTypes: []string{"remove"},
		},
		{
			name: "detect updated env var value",
			currentEnv: []corev1.EnvVar{
				{Name: "MY_VAR", Value: "old-value"},
			},
			targetEnv: []TargetEnvVar{
				{Name: "MY_VAR", Type: "value", Value: "new-value"},
			},
			expectedCount: 1,
			expectedTypes: []string{"update"},
		},
		{
			name: "no changes when values match",
			currentEnv: []corev1.EnvVar{
				{Name: "MY_VAR", Value: "same-value"},
			},
			targetEnv: []TargetEnvVar{
				{Name: "MY_VAR", Type: "value", Value: "same-value"},
			},
			expectedCount: 0,
			expectedTypes: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := r.compareEnvVars(tt.currentEnv, tt.targetEnv)
			if len(changes) != tt.expectedCount {
				t.Errorf("compareEnvVars() returned %d changes, want %d", len(changes), tt.expectedCount)
			}

			// Verify change types
			for i, expectedType := range tt.expectedTypes {
				if i < len(changes) && changes[i].Type != expectedType {
					t.Errorf("change[%d].Type = %q, want %q", i, changes[i].Type, expectedType)
				}
			}
		})
	}
}

func TestFormatCurrentEnvValue(t *testing.T) {
	r := &LiqoUpgradeReconciler{}

	tests := []struct {
		name           string
		env            corev1.EnvVar
		expectedValue  string
		expectedSource string
	}{
		{
			name:           "plain value",
			env:            corev1.EnvVar{Name: "VAR", Value: "my-value"},
			expectedValue:  "my-value",
			expectedSource: envSourceValue,
		},
		{
			name: "configMap reference",
			env: corev1.EnvVar{
				Name: "VAR",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "my-configmap"},
						Key:                  "my-key",
					},
				},
			},
			expectedValue:  "my-key",
			expectedSource: "configMap:my-configmap",
		},
		{
			name: "secret reference",
			env: corev1.EnvVar{
				Name: "VAR",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
						Key:                  "my-key",
					},
				},
			},
			expectedValue:  "my-key",
			expectedSource: "secret:my-secret",
		},
		{
			name: "fieldRef",
			env: corev1.EnvVar{
				Name: "VAR",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			},
			expectedValue:  "metadata.name",
			expectedSource: envSourceFieldRef,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, source := r.formatCurrentEnvValue(tt.env)
			if value != tt.expectedValue {
				t.Errorf("formatCurrentEnvValue() value = %q, want %q", value, tt.expectedValue)
			}
			if source != tt.expectedSource {
				t.Errorf("formatCurrentEnvValue() source = %q, want %q", source, tt.expectedSource)
			}
		})
	}
}

func TestIsDynamicComponent(t *testing.T) {
	r := &LiqoUpgradeReconciler{}

	tests := []struct {
		name      string
		component ComponentSnapshot
		expected  bool
	}{
		{
			name: "virtual-kubelet is dynamic",
			component: ComponentSnapshot{
				Name:      "liqo-virtual-kubelet-cluster1",
				Namespace: "liqo",
			},
			expected: true,
		},
		{
			name: "exact virtual-kubelet name is dynamic",
			component: ComponentSnapshot{
				Name:      "liqo-virtual-kubelet",
				Namespace: "liqo",
			},
			expected: true,
		},
		{
			name: "gateway in tenant namespace is dynamic",
			component: ComponentSnapshot{
				Name:      "liqo-gateway",
				Namespace: "liqo-tenant-cluster1",
			},
			expected: true,
		},
		{
			name: "controller-manager is not dynamic",
			component: ComponentSnapshot{
				Name:      "liqo-controller-manager",
				Namespace: "liqo",
			},
			expected: false,
		},
		{
			name: "fabric is not dynamic",
			component: ComponentSnapshot{
				Name:      "liqo-fabric",
				Namespace: "liqo",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.isDynamicComponent(&tt.component)
			if result != tt.expected {
				t.Errorf("isDynamicComponent(%q) = %v, want %v", tt.component.Name, result, tt.expected)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected map[string]string
	}{
		{
			name:     "empty args",
			args:     []string{},
			expected: map[string]string{},
		},
		{
			name:     "flag with equals",
			args:     []string{"--port=8080"},
			expected: map[string]string{"port": "8080"},
		},
		{
			name:     "flag with space",
			args:     []string{"--port", "8080"},
			expected: map[string]string{"port": "8080"},
		},
		{
			name:     "boolean flag",
			args:     []string{"--verbose"},
			expected: map[string]string{"verbose": "true"},
		},
		{
			name:     "multiple flags mixed style",
			args:     []string{"--port=8080", "--host", "localhost", "--debug"},
			expected: map[string]string{"port": "8080", "host": "localhost", "debug": "true"},
		},
		{
			name:     "positional args ignored",
			args:     []string{"command", "--flag=value", "positional"},
			expected: map[string]string{"flag": "value"},
		},
		{
			name:     "real liqo controller args",
			args:     []string{"--leader-elect", "--metrics-bind-address=:8080", "--health-probe-bind-address", ":8081"},
			expected: map[string]string{"leader-elect": "true", "metrics-bind-address": ":8080", "health-probe-bind-address": ":8081"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseArgs(tt.args)
			if len(result) != len(tt.expected) {
				t.Errorf("parseArgs() returned %d flags, want %d", len(result), len(tt.expected))
			}
			for key, expectedValue := range tt.expected {
				if gotValue, ok := result[key]; !ok {
					t.Errorf("parseArgs() missing flag %q", key)
				} else if gotValue != expectedValue {
					t.Errorf("parseArgs()[%q] = %q, want %q", key, gotValue, expectedValue)
				}
			}
		})
	}
}

func TestCompareArgs(t *testing.T) {
	r := &LiqoUpgradeReconciler{}

	tests := []struct {
		name          string
		currentArgs   []string
		targetArgs    []string
		expectedCount int
		expectedTypes []string
	}{
		{
			name:          "no changes when both empty",
			currentArgs:   []string{},
			targetArgs:    []string{},
			expectedCount: 0,
			expectedTypes: []string{},
		},
		{
			name:          "detect new flag",
			currentArgs:   []string{},
			targetArgs:    []string{"--new-flag=value"},
			expectedCount: 1,
			expectedTypes: []string{"add"},
		},
		{
			name:          "detect removed flag",
			currentArgs:   []string{"--old-flag=value"},
			targetArgs:    []string{},
			expectedCount: 1,
			expectedTypes: []string{"remove"},
		},
		{
			name:          "detect updated flag value",
			currentArgs:   []string{"--port=8080"},
			targetArgs:    []string{"--port=9090"},
			expectedCount: 1,
			expectedTypes: []string{"update"},
		},
		{
			name:          "no changes when flags match",
			currentArgs:   []string{"--port=8080", "--debug"},
			targetArgs:    []string{"--port=8080", "--debug"},
			expectedCount: 0,
			expectedTypes: []string{},
		},
		{
			name:          "mixed changes",
			currentArgs:   []string{"--keep=same", "--remove=old", "--update=v1"},
			targetArgs:    []string{"--keep=same", "--add=new", "--update=v2"},
			expectedCount: 3, // add, remove, update
			expectedTypes: []string{"add", "remove", "update"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := r.compareArgs(tt.currentArgs, tt.targetArgs)
			if len(changes) != tt.expectedCount {
				t.Errorf("compareArgs() returned %d changes, want %d", len(changes), tt.expectedCount)
			}

			// Count change types
			typeCounts := make(map[string]int)
			for _, change := range changes {
				typeCounts[change.Type]++
			}

			for _, expectedType := range tt.expectedTypes {
				if typeCounts[expectedType] == 0 {
					t.Errorf("compareArgs() missing change type %q", expectedType)
				}
			}
		})
	}
}

func TestBuildUpgradePlan(t *testing.T) {
	r := &LiqoUpgradeReconciler{}

	// Helper to create target component with image
	makeTargetComponent := func(name, kind, namespace, repo, tag string) TargetComponentDescriptor {
		comp := TargetComponentDescriptor{
			Name:      name,
			Kind:      kind,
			Namespace: namespace,
		}
		comp.Image.Repository = repo
		comp.Image.Tag = tag
		return comp
	}

	tests := []struct {
		name           string
		snapshot       *ClusterSnapshot
		descriptor     *TargetDescriptor
		expectedCreate int
		expectedUpdate int
		expectedDelete int
	}{
		{
			name: "no changes when everything matches",
			snapshot: &ClusterSnapshot{
				Version: "v1.0.0",
				Components: []ComponentSnapshot{
					{Name: "liqo-controller-manager", Kind: "Deployment", Namespace: "liqo", Exists: true, Image: "ghcr.io/liqotech/liqo-controller-manager:v1.0.0"},
				},
			},
			descriptor: &TargetDescriptor{
				Version: "v1.0.0",
				Components: []TargetComponentDescriptor{
					makeTargetComponent("liqo-controller-manager", "Deployment", "liqo", "ghcr.io/liqotech/liqo-controller-manager", "v1.0.0"),
				},
			},
			expectedCreate: 0,
			expectedUpdate: 0,
			expectedDelete: 0,
		},
		{
			name: "detect image update",
			snapshot: &ClusterSnapshot{
				Version: "v1.0.0",
				Components: []ComponentSnapshot{
					{Name: "liqo-controller-manager", Kind: "Deployment", Namespace: "liqo", Exists: true, Image: "ghcr.io/liqotech/liqo-controller-manager:v1.0.0"},
				},
			},
			descriptor: &TargetDescriptor{
				Version: "v1.0.1",
				Components: []TargetComponentDescriptor{
					makeTargetComponent("liqo-controller-manager", "Deployment", "liqo", "ghcr.io/liqotech/liqo-controller-manager", "v1.0.1"),
				},
			},
			expectedCreate: 0,
			expectedUpdate: 1,
			expectedDelete: 0,
		},
		{
			name: "detect new component to create",
			snapshot: &ClusterSnapshot{
				Version:    "v1.0.0",
				Components: []ComponentSnapshot{},
			},
			descriptor: &TargetDescriptor{
				Version: "v1.0.1",
				Components: []TargetComponentDescriptor{
					makeTargetComponent("new-component", "Deployment", "liqo", "ghcr.io/liqotech/new-component", "v1.0.1"),
				},
			},
			expectedCreate: 1,
			expectedUpdate: 0,
			expectedDelete: 0,
		},
		{
			name: "detect component to delete",
			snapshot: &ClusterSnapshot{
				Version: "v1.0.0",
				Components: []ComponentSnapshot{
					{Name: "old-component", Kind: "Deployment", Namespace: "liqo", Exists: true, Image: "ghcr.io/liqotech/old:v1.0.0"},
				},
			},
			descriptor: &TargetDescriptor{
				Version:    "v1.0.1",
				Components: []TargetComponentDescriptor{},
			},
			expectedCreate: 0,
			expectedUpdate: 0,
			expectedDelete: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := r.buildUpgradePlan(context.Background(), tt.snapshot, tt.descriptor)
			if len(plan.ToCreate) != tt.expectedCreate {
				t.Errorf("buildUpgradePlan() ToCreate = %d, want %d", len(plan.ToCreate), tt.expectedCreate)
			}
			if len(plan.ToUpdate) != tt.expectedUpdate {
				t.Errorf("buildUpgradePlan() ToUpdate = %d, want %d", len(plan.ToUpdate), tt.expectedUpdate)
			}
			if len(plan.ToDelete) != tt.expectedDelete {
				t.Errorf("buildUpgradePlan() ToDelete = %d, want %d", len(plan.ToDelete), tt.expectedDelete)
			}
		})
	}
}
