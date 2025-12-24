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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// updateStatus updates the LiqoUpgrade status with the given phase and message
// Uses retry logic to handle optimistic concurrency conflicts
func (r *LiqoUpgradeReconciler) updateStatus(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, phase upgradev1alpha1.UpgradePhase, message string, additionalUpdates map[string]interface{}) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Retry with exponential backoff on conflicts
	err := wait.ExponentialBackoff(wait.Backoff{
		Duration: 10 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
		Cap:      1 * time.Second,
	}, func() (bool, error) {
		// Get a fresh copy of the resource
		fresh := &upgradev1alpha1.LiqoUpgrade{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		}, fresh); err != nil {
			if errors.IsNotFound(err) {
				// Resource was deleted, stop retrying
				return true, err
			}
			// Temporary error, retry
			return false, nil
		}

		// Apply status updates to the fresh copy
		fresh.Status.Phase = phase
		fresh.Status.Message = message
		fresh.Status.LastUpdated = metav1.Now()

		if additionalUpdates != nil {
			if previousVersion, ok := additionalUpdates["previousVersion"].(string); ok {
				fresh.Status.PreviousVersion = previousVersion
			}
			if lastSuccessfulPhase, ok := additionalUpdates["lastSuccessfulPhase"].(upgradev1alpha1.UpgradePhase); ok {
				fresh.Status.LastSuccessfulPhase = lastSuccessfulPhase
			}
			if rolledBack, ok := additionalUpdates["rolledBack"].(bool); ok {
				fresh.Status.RolledBack = rolledBack
			}
			if conditions, ok := additionalUpdates["conditions"].([]metav1.Condition); ok {
				fresh.Status.Conditions = conditions
			}
			if snapshotConfigMap, ok := additionalUpdates["snapshotConfigMap"].(string); ok {
				fresh.Status.SnapshotConfigMap = snapshotConfigMap
			}
			if planConfigMap, ok := additionalUpdates["planConfigMap"].(string); ok {
				fresh.Status.PlanConfigMap = planConfigMap
			}
			if planReady, ok := additionalUpdates["planReady"].(bool); ok {
				fresh.Status.PlanReady = planReady
			}
			if currentStage, ok := additionalUpdates["currentStage"].(int); ok {
				fresh.Status.CurrentStage = currentStage
			}
			// Note: BackupReady and BackupName fields are planned for future implementation
			// if backupReady, ok := additionalUpdates["backupReady"].(bool); ok {
			// 	fresh.Status.BackupReady = backupReady
			// }
			// if backupName, ok := additionalUpdates["backupName"].(string); ok {
			// 	fresh.Status.BackupName = backupName
			// }
		}

		// Attempt the status update
		if err := r.Status().Update(ctx, fresh); err != nil {
			if errors.IsConflict(err) {
				// Conflict detected, retry with a fresh copy
				logger.V(1).Info("Conflict detected during status update, retrying...")
				return false, nil
			}
			// Other error, stop retrying
			return true, err
		}

		// Success, update the reference to the fresh copy
		upgrade.Status = fresh.Status
		upgrade.ResourceVersion = fresh.ResourceVersion
		return true, nil
	})

	if err != nil {
		logger.Error(err, "Failed to update status after retries")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// fail marks the upgrade as failed with the given message
func (r *LiqoUpgradeReconciler) fail(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, message string) (ctrl.Result, error) {
	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, message, nil)
}

// handleDeletion handles the deletion of a LiqoUpgrade resource
func (r *LiqoUpgradeReconciler) handleDeletion(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(upgrade, finalizerName) {
		// Clean up jobs
		jobPrefixes := []string{crdUpgradeJobPrefix, controllerManagerUpgradePrefix, networkFabricUpgradePrefix, rollbackJobPrefix}
		for _, prefix := range jobPrefixes {
			jobName := fmt.Sprintf("%s-%s", prefix, upgrade.Name)
			job := &batchv1.Job{}
			err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job)
			if err == nil {
				if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
					logger.Error(err, "Failed to delete job", "jobName", jobName)
				}
			}
		}

		controllerutil.RemoveFinalizer(upgrade, finalizerName)
		if err := r.Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// int32Ptr returns a pointer to an int32
func int32Ptr(i int32) *int32 {
	return &i
}

// updateStatusCondition updates a condition in the LiqoUpgrade status with retry logic
func (r *LiqoUpgradeReconciler) updateStatusCondition(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, condition metav1.Condition) error {
	logger := log.FromContext(ctx)

	// Retry with exponential backoff on conflicts
	return wait.ExponentialBackoff(wait.Backoff{
		Duration: 10 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
		Cap:      1 * time.Second,
	}, func() (bool, error) {
		// Get a fresh copy of the resource
		fresh := &upgradev1alpha1.LiqoUpgrade{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      upgrade.Name,
			Namespace: upgrade.Namespace,
		}, fresh); err != nil {
			if errors.IsNotFound(err) {
				// Resource was deleted, stop retrying
				return true, err
			}
			// Temporary error, retry
			return false, nil
		}

		// Find and update or append condition in the fresh copy
		found := false
		for i, c := range fresh.Status.Conditions {
			if c.Type == condition.Type {
				fresh.Status.Conditions[i] = condition
				found = true
				break
			}
		}
		if !found {
			fresh.Status.Conditions = append(fresh.Status.Conditions, condition)
		}

		// Attempt the status update
		if err := r.Status().Update(ctx, fresh); err != nil {
			if errors.IsConflict(err) {
				// Conflict detected, retry with a fresh copy
				logger.V(1).Info("Conflict detected during condition update, retrying...")
				return false, nil
			}
			// Other error, stop retrying
			return true, err
		}

		// Success, update the reference to the fresh copy
		upgrade.Status = fresh.Status
		upgrade.ResourceVersion = fresh.ResourceVersion
		return true, nil
	})
}
