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
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClusterSnapshotSerialization(t *testing.T) {
	snapshot := &ClusterSnapshot{
		Version:   "v1.0.0",
		Timestamp: metav1.Now(),
		Components: []ComponentSnapshot{
			{
				Name:      "liqo-controller-manager",
				Kind:      "Deployment",
				Namespace: "liqo",
				Exists:    true,
				Image:     "ghcr.io/liqotech/liqo-controller-manager:v1.0.0",
			},
		},
		CRDs: []CRDSnapshot{
			{
				Name:           "foreignclusters.liqo.io",
				Group:          "liqo.io",
				Versions:       []string{"v1beta1"},
				StorageVersion: "v1beta1",
			},
		},
	}

	// Test serialization
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Failed to marshal ClusterSnapshot: %v", err)
	}

	// Test deserialization
	var restored ClusterSnapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Failed to unmarshal ClusterSnapshot: %v", err)
	}

	// Verify fields
	if restored.Version != snapshot.Version {
		t.Errorf("Version = %q, want %q", restored.Version, snapshot.Version)
	}
	if len(restored.Components) != len(snapshot.Components) {
		t.Errorf("Components count = %d, want %d", len(restored.Components), len(snapshot.Components))
	}
	if len(restored.CRDs) != len(snapshot.CRDs) {
		t.Errorf("CRDs count = %d, want %d", len(restored.CRDs), len(snapshot.CRDs))
	}
}

func TestTargetDescriptorSerialization(t *testing.T) {
	descriptor := &TargetDescriptor{
		Version: "v1.0.1",
		Components: []TargetComponentDescriptor{
			{
				Name:          "liqo-controller-manager",
				Kind:          "Deployment",
				Namespace:     "liqo",
				ContainerName: "controller-manager",
			},
		},
	}
	descriptor.Components[0].Image.Repository = "ghcr.io/liqotech/liqo-controller-manager"
	descriptor.Components[0].Image.Tag = "v1.0.1"

	// Test serialization
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("Failed to marshal TargetDescriptor: %v", err)
	}

	// Test deserialization
	var restored TargetDescriptor
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Failed to unmarshal TargetDescriptor: %v", err)
	}

	// Verify fields
	if restored.Version != descriptor.Version {
		t.Errorf("Version = %q, want %q", restored.Version, descriptor.Version)
	}
	if len(restored.Components) != len(descriptor.Components) {
		t.Errorf("Components count = %d, want %d", len(restored.Components), len(descriptor.Components))
	}
	if restored.Components[0].Image.Tag != "v1.0.1" {
		t.Errorf("Image.Tag = %q, want %q", restored.Components[0].Image.Tag, "v1.0.1")
	}
}

func TestUpgradePlanSerialization(t *testing.T) {
	plan := &UpgradePlan{
		Version: "v1.0.1",
		ToCreate: []PlannedComponent{
			{Name: "new-component", Kind: "Deployment", Namespace: "liqo"},
		},
		ToUpdate: []PlannedComponent{
			{Name: "liqo-controller-manager", Kind: "Deployment", Namespace: "liqo", CurrentImage: "v1.0.0", TargetImage: "v1.0.1"},
		},
		ToDelete: []PlannedComponent{
			{Name: "old-component", Kind: "Deployment", Namespace: "liqo"},
		},
	}

	// Test serialization
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Failed to marshal UpgradePlan: %v", err)
	}

	// Test deserialization
	var restored UpgradePlan
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Failed to unmarshal UpgradePlan: %v", err)
	}

	// Verify fields
	if restored.Version != plan.Version {
		t.Errorf("Version = %q, want %q", restored.Version, plan.Version)
	}
	if len(restored.ToCreate) != 1 {
		t.Errorf("ToCreate count = %d, want 1", len(restored.ToCreate))
	}
	if len(restored.ToUpdate) != 1 {
		t.Errorf("ToUpdate count = %d, want 1", len(restored.ToUpdate))
	}
	if len(restored.ToDelete) != 1 {
		t.Errorf("ToDelete count = %d, want 1", len(restored.ToDelete))
	}
}

func TestLiqoComponentsDefinition(t *testing.T) {
	// Verify that core Liqo components are defined
	expectedComponents := map[string]string{
		"liqo-controller-manager": "Deployment",
		"liqo-crd-replicator":     "Deployment",
		"liqo-webhook":            "Deployment",
		"liqo-ipam":               "Deployment",
		"liqo-proxy":              "Deployment",
		"liqo-telemetry":          "CronJob",
		"liqo-fabric":             "DaemonSet",
		"liqo-gateway":            "Deployment",
	}

	componentMap := make(map[string]string)
	for _, comp := range liqoComponents {
		componentMap[comp.Name] = comp.Kind
	}

	for name, expectedKind := range expectedComponents {
		kind, exists := componentMap[name]
		if !exists {
			t.Errorf("Expected component %q not found in liqoComponents", name)
		} else if kind != expectedKind {
			t.Errorf("Component %q has kind %q, want %q", name, kind, expectedKind)
		}
	}
}

func TestEnvChangeSerialization(t *testing.T) {
	changes := []EnvChange{
		{Type: "add", Name: "NEW_VAR", NewValue: "value", NewSource: "value"},
		{Type: "update", Name: "UPDATED_VAR", OldValue: "old", NewValue: "new", OldSource: "value", NewSource: "value"},
		{Type: "remove", Name: "OLD_VAR", OldValue: "removed", OldSource: "value"},
	}

	// Test serialization
	data, err := json.Marshal(changes)
	if err != nil {
		t.Fatalf("Failed to marshal EnvChanges: %v", err)
	}

	// Test deserialization
	var restored []EnvChange
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Failed to unmarshal EnvChanges: %v", err)
	}

	if len(restored) != 3 {
		t.Errorf("EnvChanges count = %d, want 3", len(restored))
	}

	// Verify types
	expectedTypes := []string{"add", "update", "remove"}
	for i, expected := range expectedTypes {
		if restored[i].Type != expected {
			t.Errorf("EnvChange[%d].Type = %q, want %q", i, restored[i].Type, expected)
		}
	}
}

func TestFlagChangeSerialization(t *testing.T) {
	changes := []FlagChange{
		{Type: "add", Flag: "--new-flag", NewValue: "value"},
		{Type: "update", Flag: "--updated-flag", OldValue: "old", NewValue: "new"},
		{Type: "remove", Flag: "--old-flag", OldValue: "removed"},
	}

	// Test serialization
	data, err := json.Marshal(changes)
	if err != nil {
		t.Fatalf("Failed to marshal FlagChanges: %v", err)
	}

	// Test deserialization
	var restored []FlagChange
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Failed to unmarshal FlagChanges: %v", err)
	}

	if len(restored) != 3 {
		t.Errorf("FlagChanges count = %d, want 3", len(restored))
	}
}
