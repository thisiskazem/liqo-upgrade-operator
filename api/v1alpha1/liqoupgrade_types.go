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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type UpgradeStrategy string

const (
	StrategySequential UpgradeStrategy = "Sequential"
	StrategyParallel   UpgradeStrategy = "Parallel"
)

// DefaultImageRegistry is the default container registry for Liqo images
const DefaultImageRegistry = "ghcr.io/liqotech"

type LiqoUpgradeSpec struct {
	TargetVersion string           `json:"targetVersion"`
	Namespace     string           `json:"namespace,omitempty"`
	AutoRollback  *bool            `json:"autoRollback,omitempty"`
	Strategy      *UpgradeStrategy `json:"strategy,omitempty"`
	DryRun        bool             `json:"dryRun,omitempty"`
	// ImageRegistry is the container registry for Liqo images (default: ghcr.io/liqotech)
	ImageRegistry string `json:"imageRegistry,omitempty"`
}

// GetImageRegistry returns the image registry, defaulting to ghcr.io/liqotech if not specified
func (s *LiqoUpgradeSpec) GetImageRegistry() string {
	if s.ImageRegistry == "" {
		return DefaultImageRegistry
	}
	return s.ImageRegistry
}

type UpgradePhase string

const (
	PhaseNone              UpgradePhase = ""
	PhasePending           UpgradePhase = "Pending"
	PhaseValidating        UpgradePhase = "Validating"
	PhaseCRDs              UpgradePhase = "UpgradingCRDs"
	PhaseControllerManager UpgradePhase = "UpgradingControllerManager"
	PhaseNetworkFabric     UpgradePhase = "UpgradingNetworkFabric"
	PhaseVerifying         UpgradePhase = "Verifying"
	PhaseRollingBack       UpgradePhase = "RollingBack"
	PhaseCompleted         UpgradePhase = "Completed"
	PhaseFailed            UpgradePhase = "Failed"
)

type ConditionType string

const (
	ConditionCompatible       ConditionType = "Compatible"
	ConditionHealthy          ConditionType = "Healthy"
	ConditionRollbackRequired ConditionType = "RollbackRequired"
	ConditionCRDWarnings      ConditionType = "CRDWarnings"
)

type LiqoUpgradeStatus struct {
	Phase               UpgradePhase       `json:"phase,omitempty"`
	Message             string             `json:"message,omitempty"`
	LastUpdated         metav1.Time        `json:"lastUpdated,omitempty"`
	PreviousVersion     string             `json:"previousVersion,omitempty"`
	SnapshotConfigMap   string             `json:"snapshotConfigMap,omitempty"`
	PlanConfigMap       string             `json:"planConfigMap,omitempty"`
	PlanReady           bool               `json:"planReady,omitempty"`
	LastSuccessfulPhase UpgradePhase       `json:"lastSuccessfulPhase,omitempty"`
	CurrentStage        int                `json:"currentStage,omitempty"`
	TotalStages         int                `json:"totalStages,omitempty"`
	RolledBack          bool               `json:"rolledBack,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.currentStage`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type LiqoUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LiqoUpgradeSpec   `json:"spec"`
	Status            LiqoUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type LiqoUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiqoUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiqoUpgrade{}, &LiqoUpgradeList{})
}
