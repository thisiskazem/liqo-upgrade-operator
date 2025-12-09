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
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// Constants for environment variable source types
const (
	envSourceValue    = "value"
	envSourceFieldRef = "fieldRef"
)

// Component name constants
const (
	virtualKubeletPrefix = "liqo-virtual-kubelet"
)

// UpgradePlan represents the computed upgrade plan comparing snapshot vs descriptor
type UpgradePlan struct {
	Version  string             `json:"version"`
	ToCreate []PlannedComponent `json:"toCreate,omitempty"`
	ToUpdate []PlannedComponent `json:"toUpdate,omitempty"`
	ToDelete []PlannedComponent `json:"toDelete,omitempty"`
}

// PlannedComponent represents a component that needs to be created, updated, or deleted
type PlannedComponent struct {
	Name          string       `json:"name"`
	Namespace     string       `json:"namespace"`
	Kind          string       `json:"kind"`
	ContainerName string       `json:"containerName,omitempty"`
	CurrentImage  string       `json:"currentImage,omitempty"`
	TargetImage   string       `json:"targetImage"`
	EnvChanges    []EnvChange  `json:"envChanges,omitempty"`
	FlagChanges   []FlagChange `json:"flagChanges,omitempty"`
}

// EnvChange represents a change to an environment variable
type EnvChange struct {
	Type      string `json:"type"` // "add", "update", "remove"
	Name      string `json:"name"`
	OldValue  string `json:"oldValue,omitempty"`
	NewValue  string `json:"newValue,omitempty"`
	OldSource string `json:"oldSource,omitempty"` // For ConfigMap/Secret refs
	NewSource string `json:"newSource,omitempty"`
}

// FlagChange represents a change to a command-line flag
type FlagChange struct {
	Type     string `json:"type"` // "add", "update", "remove"
	Flag     string `json:"flag"`
	OldValue string `json:"oldValue,omitempty"`
	NewValue string `json:"newValue,omitempty"`
}

// buildUpgradePlan compares snapshot (current state) with target descriptor (desired state)
// and generates a plan of what needs to be created, updated, or deleted
func (r *LiqoUpgradeReconciler) buildUpgradePlan(ctx context.Context, snapshot *ClusterSnapshot, descriptor *TargetDescriptor) *UpgradePlan {
	logger := log.FromContext(ctx)
	logger.Info("Building upgrade plan", "currentVersion", snapshot.Version, "targetVersion", descriptor.Version)

	plan := &UpgradePlan{
		Version:  descriptor.Version,
		ToCreate: []PlannedComponent{},
		ToUpdate: []PlannedComponent{},
		ToDelete: []PlannedComponent{},
	}

	// Build maps for efficient lookup
	snapshotMap := make(map[string]*ComponentSnapshot)
	for i := range snapshot.Components {
		comp := &snapshot.Components[i]
		key := fmt.Sprintf("%s/%s/%s", comp.Kind, comp.Namespace, comp.Name)
		snapshotMap[key] = comp
	}

	descriptorMap := make(map[string]*TargetComponentDescriptor)
	for i := range descriptor.Components {
		comp := &descriptor.Components[i]
		key := fmt.Sprintf("%s/%s/%s", comp.Kind, comp.Namespace, comp.Name)
		descriptorMap[key] = comp
	}

	// 1. Find components to create or update (exist in descriptor)
	for key, targetComp := range descriptorMap {
		currentComp, existsInSnapshot := snapshotMap[key]

		if !existsInSnapshot || !currentComp.Exists {
			// Component doesn't exist in current cluster -> CREATE
			plan.ToCreate = append(plan.ToCreate, PlannedComponent{
				Name:          targetComp.Name,
				Namespace:     targetComp.Namespace,
				Kind:          targetComp.Kind,
				ContainerName: targetComp.ContainerName,
				TargetImage:   fmt.Sprintf("%s:%s", targetComp.Image.Repository, targetComp.Image.Tag),
				EnvChanges:    []EnvChange{},
				FlagChanges:   []FlagChange{},
			})
			logger.Info("Plan: CREATE component", "name", targetComp.Name, "kind", targetComp.Kind)
		} else {
			// Component exists -> check if UPDATE needed
			planned := r.compareComponent(currentComp, targetComp)
			if planned != nil {
				plan.ToUpdate = append(plan.ToUpdate, *planned)
				logger.Info("Plan: UPDATE component", "name", targetComp.Name, "kind", targetComp.Kind,
					"imageChange", planned.CurrentImage != planned.TargetImage,
					"envChanges", len(planned.EnvChanges),
					"flagChanges", len(planned.FlagChanges))
			}
		}
	}

	// 2. Find components to delete (exist in snapshot but not in descriptor)
	for key, currentComp := range snapshotMap {
		_, existsInDescriptor := descriptorMap[key]
		if !existsInDescriptor && currentComp.Exists {
			// Skip dynamic components (virtual-kubelet, tenant gateways)
			if r.isDynamicComponent(currentComp) {
				logger.Info("Skipping dynamic component in plan", "name", currentComp.Name, "kind", currentComp.Kind)
				continue
			}

			// Component exists in cluster but not in target descriptor -> DELETE (candidate)
			plan.ToDelete = append(plan.ToDelete, PlannedComponent{
				Name:         currentComp.Name,
				Namespace:    currentComp.Namespace,
				Kind:         currentComp.Kind,
				CurrentImage: currentComp.Image,
			})
			logger.Info("Plan: DELETE candidate", "name", currentComp.Name, "kind", currentComp.Kind)
		}
	}

	logger.Info("Upgrade plan built", "toCreate", len(plan.ToCreate), "toUpdate", len(plan.ToUpdate), "toDelete", len(plan.ToDelete))
	return plan
}

// compareComponent compares a current component with its target descriptor
// Returns a PlannedComponent with all changes, or nil if no changes needed
func (r *LiqoUpgradeReconciler) compareComponent(current *ComponentSnapshot, target *TargetComponentDescriptor) *PlannedComponent {
	planned := &PlannedComponent{
		Name:          target.Name,
		Namespace:     target.Namespace,
		Kind:          target.Kind,
		ContainerName: target.ContainerName,
		CurrentImage:  current.Image,
		TargetImage:   fmt.Sprintf("%s:%s", target.Image.Repository, target.Image.Tag),
		EnvChanges:    []EnvChange{},
		FlagChanges:   []FlagChange{},
	}

	// 1. Compare images
	hasChanges := current.Image != planned.TargetImage

	// 2. Compare environment variables
	envChanges := r.compareEnvVars(current.Env, target.Env)
	if len(envChanges) > 0 {
		planned.EnvChanges = envChanges
		hasChanges = true
	}

	// 3. Compare args/flags
	flagChanges := r.compareArgs(current.Args, target.Args)
	if len(flagChanges) > 0 {
		planned.FlagChanges = flagChanges
		hasChanges = true
	}

	if !hasChanges {
		return nil // No changes needed
	}

	return planned
}

// compareEnvVars compares current env vars with target env vars
func (r *LiqoUpgradeReconciler) compareEnvVars(currentEnv []corev1.EnvVar, targetEnv []TargetEnvVar) []EnvChange {
	changes := []EnvChange{}

	// Build maps for comparison
	currentMap := make(map[string]corev1.EnvVar)
	for _, e := range currentEnv {
		currentMap[e.Name] = e
	}

	targetMap := make(map[string]TargetEnvVar)
	for _, e := range targetEnv {
		targetMap[e.Name] = e
	}

	// Check for additions and updates
	for name, targetVar := range targetMap {
		currentVar, exists := currentMap[name]
		if !exists {
			// New env var
			changes = append(changes, EnvChange{
				Type:      "add",
				Name:      name,
				NewValue:  r.formatTargetEnvValue(targetVar),
				NewSource: r.formatTargetEnvSource(targetVar),
			})
		} else {
			// Compare existing env var
			currentValue, currentSource := r.formatCurrentEnvValue(currentVar)
			targetValue := r.formatTargetEnvValue(targetVar)
			targetSource := r.formatTargetEnvSource(targetVar)

			if currentValue != targetValue || currentSource != targetSource {
				changes = append(changes, EnvChange{
					Type:      "update",
					Name:      name,
					OldValue:  currentValue,
					NewValue:  targetValue,
					OldSource: currentSource,
					NewSource: targetSource,
				})
			}
		}
	}

	// Check for removals
	for name := range currentMap {
		if _, exists := targetMap[name]; !exists {
			currentVar := currentMap[name]
			oldValue, oldSource := r.formatCurrentEnvValue(currentVar)
			changes = append(changes, EnvChange{
				Type:      "remove",
				Name:      name,
				OldValue:  oldValue,
				OldSource: oldSource,
			})
		}
	}

	return changes
}

// formatCurrentEnvValue formats a Kubernetes EnvVar for comparison
func (r *LiqoUpgradeReconciler) formatCurrentEnvValue(env corev1.EnvVar) (string, string) {
	if env.Value != "" {
		return env.Value, envSourceValue
	}
	if env.ValueFrom != nil {
		if env.ValueFrom.ConfigMapKeyRef != nil {
			return env.ValueFrom.ConfigMapKeyRef.Key,
				fmt.Sprintf("configMap:%s", env.ValueFrom.ConfigMapKeyRef.Name)
		}
		if env.ValueFrom.SecretKeyRef != nil {
			return env.ValueFrom.SecretKeyRef.Key,
				fmt.Sprintf("secret:%s", env.ValueFrom.SecretKeyRef.Name)
		}
		if env.ValueFrom.FieldRef != nil {
			return env.ValueFrom.FieldRef.FieldPath, envSourceFieldRef
		}
	}
	return "", "unknown"
}

// formatTargetEnvValue formats a TargetEnvVar for comparison
func (r *LiqoUpgradeReconciler) formatTargetEnvValue(env TargetEnvVar) string {
	switch env.Type {
	case envSourceValue:
		return env.Value
	case "configMapKeyRef":
		return env.Key
	case "secretKeyRef":
		return env.Key
	case envSourceFieldRef:
		return env.Value
	default:
		return ""
	}
}

// formatTargetEnvSource formats the source type for a TargetEnvVar
func (r *LiqoUpgradeReconciler) formatTargetEnvSource(env TargetEnvVar) string {
	switch env.Type {
	case envSourceValue:
		return envSourceValue
	case "configMapKeyRef":
		return fmt.Sprintf("configMap:%s", env.ConfigMapName)
	case "secretKeyRef":
		return fmt.Sprintf("secret:%s", env.SecretName)
	case envSourceFieldRef:
		return envSourceFieldRef
	default:
		return "unknown"
	}
}

// compareArgs compares current container args with target args
func (r *LiqoUpgradeReconciler) compareArgs(currentArgs, targetArgs []string) []FlagChange {
	changes := []FlagChange{}

	// Parse args into flag maps
	currentFlags := parseArgs(currentArgs)
	targetFlags := parseArgs(targetArgs)

	// Check for additions and updates
	for flag, targetValue := range targetFlags {
		currentValue, exists := currentFlags[flag]
		if !exists {
			changes = append(changes, FlagChange{
				Type:     "add",
				Flag:     flag,
				NewValue: targetValue,
			})
		} else if currentValue != targetValue {
			changes = append(changes, FlagChange{
				Type:     "update",
				Flag:     flag,
				OldValue: currentValue,
				NewValue: targetValue,
			})
		}
	}

	// Check for removals
	for flag, currentValue := range currentFlags {
		if _, exists := targetFlags[flag]; !exists {
			changes = append(changes, FlagChange{
				Type:     "remove",
				Flag:     flag,
				OldValue: currentValue,
			})
		}
	}

	return changes
}

// parseArgs parses command-line arguments into a flag -> value map
// Handles: --flag=value, --flag value, --bool-flag
func parseArgs(args []string) map[string]string {
	flags := make(map[string]string)

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if !strings.HasPrefix(arg, "--") {
			// Not a flag, skip (could be positional arg)
			continue
		}

		// Handle --flag=value
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			flagName := strings.TrimPrefix(parts[0], "--")
			flagValue := parts[1]
			flags[flagName] = flagValue
			continue
		}

		// Handle --flag value or --bool-flag
		flagName := strings.TrimPrefix(arg, "--")

		// Check if next arg exists and is not a flag
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			// --flag value
			flags[flagName] = args[i+1]
			i++ // Skip next arg
		} else {
			// --bool-flag (no value)
			flags[flagName] = "true"
		}
	}

	return flags
}

// isDynamicComponent checks if a component is dynamic (virtual-kubelet, tenant gateway)
func (r *LiqoUpgradeReconciler) isDynamicComponent(comp *ComponentSnapshot) bool {
	// Virtual kubelet patterns
	if len(comp.Name) >= len(virtualKubeletPrefix) && comp.Name[:len(virtualKubeletPrefix)] == virtualKubeletPrefix {
		return true
	}

	// Tenant gateway pattern (in liqo-tenant-* namespaces)
	if len(comp.Namespace) > 12 && comp.Namespace[:12] == "liqo-tenant-" {
		return true
	}

	return false
}

// createUpgradePlanConfigMap creates a ConfigMap with the upgrade plan
func (r *LiqoUpgradeReconciler) createUpgradePlanConfigMap(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, plan *UpgradePlan, namespace string) error {
	logger := log.FromContext(ctx)

	configMapName := fmt.Sprintf("liqo-upgrade-plan-%s", upgrade.Name)

	// Marshal plan to JSON
	planJSON, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal upgrade plan: %w", err)
	}

	// Create ConfigMap
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "upgrade-plan",
				"upgrade.liqo.io/upgrade":     upgrade.Name,
			},
		},
		Data: map[string]string{
			"plan.json": string(planJSON),
		},
	}

	if err := controllerutil.SetControllerReference(upgrade, configMap, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create or update the ConfigMap
	existingConfigMap := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, existingConfigMap)
	if err != nil {
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, configMap); err != nil {
				return fmt.Errorf("failed to create upgrade plan ConfigMap: %w", err)
			}
			logger.Info("Upgrade plan ConfigMap created", "name", configMapName)
		} else {
			return fmt.Errorf("failed to check upgrade plan ConfigMap: %w", err)
		}
	} else {
		// Update existing
		existingConfigMap.Data = configMap.Data
		if err := r.Update(ctx, existingConfigMap); err != nil {
			return fmt.Errorf("failed to update upgrade plan ConfigMap: %w", err)
		}
		logger.Info("Upgrade plan ConfigMap updated", "name", configMapName)
	}

	// Update upgrade status with plan reference
	upgrade.Status.PlanConfigMap = configMapName
	upgrade.Status.PlanReady = true

	return nil
}
