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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// Verification Phase
func (r *LiqoUpgradeReconciler) startVerification(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting verification phase")

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseVerifying, "Verifying upgrade", nil)
}

func (r *LiqoUpgradeReconciler) performVerification(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Performing post-upgrade verification")

	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = defaultLiqoNamespace
	}

	// Verify all components are healthy
	if err := r.verifyComponentHealth(ctx, namespace); err != nil {
		logger.Error(err, "Verification failed: components not healthy")
		return r.startRollback(ctx, upgrade, fmt.Sprintf("Verification failed: %v", err))
	}

	// Verify version
	deployedVersion, err := r.detectDeployedVersion(ctx, namespace)
	if err != nil {
		return r.startRollback(ctx, upgrade, fmt.Sprintf("Version verification failed: %v", err))
	}

	if deployedVersion != upgrade.Spec.TargetVersion {
		return r.startRollback(ctx, upgrade, fmt.Sprintf("Version mismatch after upgrade: deployed=%s, expected=%s", deployedVersion, upgrade.Spec.TargetVersion))
	}

	// Verification passed
	logger.Info("Verification passed, upgrade complete!")

	condition := metav1.Condition{
		Type:               string(upgradev1alpha1.ConditionHealthy),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "VerificationPassed",
		Message:            "All components healthy and version verified",
	}

	statusUpdates := map[string]interface{}{
		"conditions": append(upgrade.Status.Conditions, condition),
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompleted,
		fmt.Sprintf("Upgrade completed successfully: %s → %s", upgrade.Status.PreviousVersion, upgrade.Spec.TargetVersion),
		statusUpdates)
}
