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

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// ComponentSnapshot represents a snapshot of a single Liqo component
type ComponentSnapshot struct {
	Name      string                      `json:"name"`
	Kind      string                      `json:"kind"`
	Namespace string                      `json:"namespace"`
	Exists    bool                        `json:"exists"`
	Image     string                      `json:"image,omitempty"`
	Args      []string                    `json:"args,omitempty"`
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	Labels    map[string]string           `json:"labels,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// CRDSnapshot represents a snapshot of a CRD
type CRDSnapshot struct {
	Name           string   `json:"name"`
	Group          string   `json:"group"`
	Versions       []string `json:"versions"`
	StorageVersion string   `json:"storageVersion"`
}

// ClusterSnapshot represents the full snapshot of the Liqo installation
type ClusterSnapshot struct {
	Components []ComponentSnapshot `json:"components"`
	CRDs       []CRDSnapshot       `json:"crds"`
	Timestamp  metav1.Time         `json:"timestamp"`
	Version    string              `json:"version"`
}

// TargetEnvVar represents an environment variable in the target descriptor
type TargetEnvVar struct {
	Name          string `json:"name"`
	Type          string `json:"type"` // "value", "configMapKeyRef", "secretKeyRef"
	Value         string `json:"value,omitempty"`
	ConfigMapName string `json:"configMapName,omitempty"`
	SecretName    string `json:"secretName,omitempty"`
	Key           string `json:"key,omitempty"`
}

// TargetComponentDescriptor describes a component in the target Liqo version
type TargetComponentDescriptor struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Namespace     string `json:"namespace"`
	ContainerName string `json:"containerName"`
	Image         struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
	} `json:"image"`
	Args []string       `json:"args,omitempty"`
	Env  []TargetEnvVar `json:"env,omitempty"`
}

// TargetDescriptor represents the expected state of a Liqo version
type TargetDescriptor struct {
	Version    string                      `json:"version"`
	Components []TargetComponentDescriptor `json:"components"`
}

// Component definitions to inventory
var liqoComponents = []struct {
	Name      string
	Kind      string
	Namespace string
}{
	{"liqo-controller-manager", "Deployment", "liqo"},
	{"liqo-crd-replicator", "Deployment", "liqo"},
	{"liqo-webhook", "Deployment", "liqo"},
	{"liqo-ipam", "Deployment", "liqo"},
	{"liqo-proxy", "Deployment", "liqo"},
	{"liqo-telemetry", "CronJob", "liqo"},
	{"liqo-metric-agent", "Deployment", "liqo"},
	{"liqo-fabric", "DaemonSet", "liqo"},
	{"liqo-gateway", "Deployment", "liqo"},
}

// createLiveInventory builds a full snapshot of the current Liqo installation
func (r *LiqoUpgradeReconciler) createLiveInventory(ctx context.Context, namespace string, currentVersion string) (*ClusterSnapshot, error) {
	logger := log.FromContext(ctx)
	logger.Info("Creating live inventory of Liqo installation")

	snapshot := &ClusterSnapshot{
		Components: []ComponentSnapshot{},
		CRDs:       []CRDSnapshot{},
		Timestamp:  metav1.Now(),
		Version:    currentVersion,
	}

	// 1. Inventory core components
	for _, comp := range liqoComponents {
		ns := comp.Namespace
		if ns == "" {
			ns = namespace
		}

		compSnapshot, err := r.inventoryComponent(ctx, comp.Name, comp.Kind, ns)
		if err != nil {
			logger.Info("Component not found or error", "name", comp.Name, "error", err)
		}
		snapshot.Components = append(snapshot.Components, *compSnapshot)
	}

	// 2. Inventory virtual-kubelet DaemonSets (dynamic discovery)
	vkComponents, err := r.inventoryVirtualKubelets(ctx, namespace)
	if err != nil {
		logger.Error(err, "Failed to inventory virtual-kubelets")
	} else {
		snapshot.Components = append(snapshot.Components, vkComponents...)
	}

	// 3. Inventory per-tenant gateway deployments
	tenantGateways, err := r.inventoryTenantGateways(ctx)
	if err != nil {
		logger.Error(err, "Failed to inventory tenant gateways")
	} else {
		snapshot.Components = append(snapshot.Components, tenantGateways...)
	}

	// 4. Inventory CRDs
	crdSnapshots, err := r.inventoryCRDs(ctx)
	if err != nil {
		logger.Error(err, "Failed to inventory CRDs")
	} else {
		snapshot.CRDs = crdSnapshots
	}

	logger.Info("Live inventory complete", "components", len(snapshot.Components), "crds", len(snapshot.CRDs))
	return snapshot, nil
}

// inventoryComponent creates a snapshot of a single component
func (r *LiqoUpgradeReconciler) inventoryComponent(ctx context.Context, name, kind, namespace string) (*ComponentSnapshot, error) {
	snapshot := &ComponentSnapshot{
		Name:      name,
		Kind:      kind,
		Namespace: namespace,
		Exists:    false,
	}

	var podSpec *corev1.PodSpec
	var labels map[string]string

	switch kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				return snapshot, nil
			}
			return snapshot, err
		}
		snapshot.Exists = true
		podSpec = &obj.Spec.Template.Spec
		labels = obj.Labels

	case "DaemonSet":
		obj := &appsv1.DaemonSet{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				return snapshot, nil
			}
			return snapshot, err
		}
		snapshot.Exists = true
		podSpec = &obj.Spec.Template.Spec
		labels = obj.Labels

	case "CronJob":
		obj := &batchv1.CronJob{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				return snapshot, nil
			}
			return snapshot, err
		}
		snapshot.Exists = true
		podSpec = &obj.Spec.JobTemplate.Spec.Template.Spec
		labels = obj.Labels

	default:
		return snapshot, fmt.Errorf("unsupported kind: %s", kind)
	}

	// Extract container info (use first container as primary)
	if podSpec != nil && len(podSpec.Containers) > 0 {
		container := podSpec.Containers[0]
		snapshot.Image = container.Image
		snapshot.Args = container.Args
		snapshot.Env = container.Env
		snapshot.Resources = container.Resources
	}

	snapshot.Labels = labels
	return snapshot, nil
}

// inventoryVirtualKubelets discovers all virtual-kubelet DaemonSets/Deployments
func (r *LiqoUpgradeReconciler) inventoryVirtualKubelets(ctx context.Context, namespace string) ([]ComponentSnapshot, error) {
	var snapshots []ComponentSnapshot

	// List all DaemonSets with liqo labels
	dsList := &appsv1.DaemonSetList{}
	err := r.List(ctx, dsList, client.InNamespace(namespace), client.MatchingLabels{"app.kubernetes.io/part-of": "liqo"})
	if err != nil {
		return snapshots, err
	}

	for _, ds := range dsList.Items {
		// Look for virtual-kubelet pattern
		if len(ds.Name) > 0 && (ds.Name == "liqo-virtual-kubelet" ||
			(len(ds.Name) > 20 && ds.Name[:20] == "liqo-virtual-kubelet")) {
			snapshot := &ComponentSnapshot{
				Name:      ds.Name,
				Kind:      "DaemonSet",
				Namespace: ds.Namespace,
				Exists:    true,
				Labels:    ds.Labels,
			}
			if len(ds.Spec.Template.Spec.Containers) > 0 {
				container := ds.Spec.Template.Spec.Containers[0]
				snapshot.Image = container.Image
				snapshot.Args = container.Args
				snapshot.Env = container.Env
				snapshot.Resources = container.Resources
			}
			snapshots = append(snapshots, *snapshot)
		}
	}

	return snapshots, nil
}

// inventoryTenantGateways discovers gateway deployments in liqo-tenant-* namespaces
func (r *LiqoUpgradeReconciler) inventoryTenantGateways(ctx context.Context) ([]ComponentSnapshot, error) {
	var snapshots []ComponentSnapshot

	// List all namespaces
	nsList := &corev1.NamespaceList{}
	err := r.List(ctx, nsList)
	if err != nil {
		return snapshots, err
	}

	// Find liqo-tenant-* namespaces
	for _, ns := range nsList.Items {
		if len(ns.Name) > 12 && ns.Name[:12] == "liqo-tenant-" {
			// List gateway deployments in this tenant namespace
			deployList := &appsv1.DeploymentList{}
			err := r.List(ctx, deployList, client.InNamespace(ns.Name), client.MatchingLabels{"networking.liqo.io/component": "gateway"})
			if err != nil {
				continue
			}

			for _, deploy := range deployList.Items {
				snapshot := &ComponentSnapshot{
					Name:      deploy.Name,
					Kind:      "Deployment",
					Namespace: deploy.Namespace,
					Exists:    true,
					Labels:    deploy.Labels,
				}
				if len(deploy.Spec.Template.Spec.Containers) > 0 {
					container := deploy.Spec.Template.Spec.Containers[0]
					snapshot.Image = container.Image
					snapshot.Args = container.Args
					snapshot.Env = container.Env
					snapshot.Resources = container.Resources
				}
				snapshots = append(snapshots, *snapshot)
			}
		}
	}

	return snapshots, nil
}

// inventoryCRDs lists all Liqo CRDs
func (r *LiqoUpgradeReconciler) inventoryCRDs(ctx context.Context) ([]CRDSnapshot, error) {
	var snapshots []CRDSnapshot

	crdList := &apiextensionsv1.CustomResourceDefinitionList{}
	err := r.List(ctx, crdList)
	if err != nil {
		return snapshots, err
	}

	// Filter for Liqo CRDs (*.liqo.io)
	for _, crd := range crdList.Items {
		if len(crd.Spec.Group) > 8 && crd.Spec.Group[len(crd.Spec.Group)-8:] == ".liqo.io" {
			snapshot := CRDSnapshot{
				Name:     crd.Name,
				Group:    crd.Spec.Group,
				Versions: []string{},
			}

			// Extract version info
			for _, ver := range crd.Spec.Versions {
				snapshot.Versions = append(snapshot.Versions, ver.Name)
				if ver.Storage {
					snapshot.StorageVersion = ver.Name
				}
			}

			snapshots = append(snapshots, snapshot)
		}
	}

	return snapshots, nil
}

// createSnapshotConfigMap creates a ConfigMap with the full snapshot
func (r *LiqoUpgradeReconciler) createSnapshotConfigMap(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, snapshot *ClusterSnapshot, namespace string) error {
	logger := log.FromContext(ctx)

	configMapName := fmt.Sprintf("liqo-upgrade-snapshot-%s", upgrade.Name)

	// Marshal snapshot to JSON
	snapshotJSON, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	// Create ConfigMap
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "snapshot",
				"upgrade.liqo.io/upgrade":     upgrade.Name,
			},
		},
		Data: map[string]string{
			"snapshot.json": string(snapshotJSON),
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
				return fmt.Errorf("failed to create snapshot ConfigMap: %w", err)
			}
			logger.Info("Snapshot ConfigMap created", "name", configMapName)
		} else {
			return fmt.Errorf("failed to check snapshot ConfigMap: %w", err)
		}
	} else {
		// Update existing
		existingConfigMap.Data = configMap.Data
		if err := r.Update(ctx, existingConfigMap); err != nil {
			return fmt.Errorf("failed to update snapshot ConfigMap: %w", err)
		}
		logger.Info("Snapshot ConfigMap updated", "name", configMapName)
	}

	// Update upgrade status with snapshot reference
	upgrade.Status.SnapshotConfigMap = configMapName

	return nil
}

// loadTargetDescriptor loads the target version descriptor from ConfigMap
func (r *LiqoUpgradeReconciler) loadTargetDescriptor(ctx context.Context, targetVersion, namespace string) (*TargetDescriptor, error) {
	logger := log.FromContext(ctx)

	configMapName := "liqo-target-descriptors"
	configMap := &corev1.ConfigMap{}

	err := r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}, configMap)
	if err != nil {
		return nil, fmt.Errorf("failed to get target descriptors ConfigMap: %w", err)
	}

	// Look for descriptor with target version as key
	descriptorJSON, ok := configMap.Data[targetVersion+".json"]
	if !ok {
		return nil, fmt.Errorf("no descriptor found for target version %s (available versions: check ConfigMap %s)", targetVersion, configMapName)
	}

	var descriptor TargetDescriptor
	if err := json.Unmarshal([]byte(descriptorJSON), &descriptor); err != nil {
		return nil, fmt.Errorf("failed to parse target descriptor for %s: %w", targetVersion, err)
	}

	logger.Info("Loaded target descriptor", "version", descriptor.Version, "components", len(descriptor.Components))
	return &descriptor, nil
}
