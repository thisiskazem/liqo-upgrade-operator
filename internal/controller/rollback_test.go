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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

func TestBuildRollbackJob(t *testing.T) {
	tests := []struct {
		name            string
		upgrade         *upgradev1alpha1.LiqoUpgrade
		expectedJobName string
		expectedImage   string
		validateScript  func(t *testing.T, script string)
	}{
		{
			name: "rollback after controller manager failure",
			upgrade: &upgradev1alpha1.LiqoUpgrade{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-upgrade",
					Namespace: "default",
				},
				Spec: upgradev1alpha1.LiqoUpgradeSpec{
					TargetVersion: "v1.0.1",
					Namespace:     "liqo",
				},
				Status: upgradev1alpha1.LiqoUpgradeStatus{
					Phase:               upgradev1alpha1.PhaseControllerManager,
					PreviousVersion:     "v1.0.0",
					LastSuccessfulPhase: upgradev1alpha1.PhaseCRDs,
				},
			},
			expectedJobName: "liqo-rollback-test-upgrade",
			expectedImage:   "bitnami/kubectl:latest",
			validateScript: func(t *testing.T, script string) {
				// Check if script contains rollback commands
				if !contains(script, "PREVIOUS_VERSION=\"v1.0.0\"") {
					t.Error("Script should contain PREVIOUS_VERSION")
				}
				if !contains(script, "FAILED_PHASE=\"UpgradingControllerManager\"") {
					t.Error("Script should contain FAILED_PHASE")
				}
			},
		},
		{
			name: "rollback with custom image registry",
			upgrade: &upgradev1alpha1.LiqoUpgrade{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-upgrade-custom",
					Namespace: "default",
				},
				Spec: upgradev1alpha1.LiqoUpgradeSpec{
					TargetVersion: "v1.0.1",
					Namespace:     "liqo",
					ImageRegistry: "my-registry.com/liqo",
				},
				Status: upgradev1alpha1.LiqoUpgradeStatus{
					Phase:               upgradev1alpha1.PhaseNetworkFabric,
					PreviousVersion:     "v1.0.0",
					LastSuccessfulPhase: upgradev1alpha1.PhaseControllerManager,
				},
			},
			expectedJobName: "liqo-rollback-test-upgrade-custom",
			expectedImage:   "bitnami/kubectl:latest",
			validateScript: func(t *testing.T, script string) {
				if !contains(script, "IMAGE_REGISTRY=\"my-registry.com/liqo\"") {
					t.Error("Script should contain custom IMAGE_REGISTRY")
				}
				if !contains(script, "ROLLBACK_NETWORK=true") {
					t.Error("Script should rollback network when NetworkFabric phase fails")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &LiqoUpgradeReconciler{}
			job := r.buildRollbackJob(tt.upgrade)

			// Verify job name
			if job.Name != tt.expectedJobName {
				t.Errorf("Job name = %q, want %q", job.Name, tt.expectedJobName)
			}

			// Verify namespace
			if job.Namespace != tt.upgrade.Spec.Namespace {
				t.Errorf("Job namespace = %q, want %q", job.Namespace, tt.upgrade.Spec.Namespace)
			}

			// Verify container image
			if len(job.Spec.Template.Spec.Containers) > 0 {
				container := job.Spec.Template.Spec.Containers[0]
				if container.Image != tt.expectedImage {
					t.Errorf("Container image = %q, want %q", container.Image, tt.expectedImage)
				}

				// Verify command
				if len(container.Command) < 3 {
					t.Error("Container command should have at least 3 elements")
				} else {
					// The script is in Command[2]
					script := container.Command[2]
					tt.validateScript(t, script)
				}
			} else {
				t.Error("Job should have at least one container")
			}

			// Verify TTL
			if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 1800 {
				t.Error("Job should have TTLSecondsAfterFinished set to 1800")
			}

			// Verify BackoffLimit
			if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
				t.Error("Job should have BackoffLimit set to 0")
			}
		})
	}
}

// Helper functions
func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && s != substr &&
		(s == substr || len(s) > len(substr) &&
			(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
				findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 1; i < len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
