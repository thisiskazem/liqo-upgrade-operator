# Liqo Upgrade Operator - Testing Examples

This directory contains example configurations and helper scripts for testing the liqo-upgrade-operator in a local cluster.

## Prerequisites

Before testing the upgrade operator, ensure you have:

1. A Kubernetes cluster (local or remote) with Liqo installed
2. `kubectl` configured to access your cluster
3. The liqo-upgrade-operator installed in your cluster

## Quick Start

### 1. Install the Operator

```bash
# From the repository root
make install
kubectl apply -f config/rbac/upgrade-rbac.yaml
kubectl apply -f config/default/compatibility-configmap.yaml
kubectl apply -f config/default/target-descriptors-configmap.yaml

# Build and deploy the operator
make docker-build docker-push IMG=<your-registry>/liqo-upgrade-controller:<tag>
make deploy IMG=<your-registry>/liqo-upgrade-controller:<tag>
```

### 2. Set Up Test Environment

The setup script will automatically detect your Liqo installation and create the necessary ConfigMaps:

```bash
cd examples
./setup-test-environment.sh
```

This script will:
- Verify Liqo is installed
- Extract the CLUSTER_ID from your liqo-controller-manager deployment
- Create the `liqo-cluster-id` ConfigMap if it doesn't exist
- Verify all required ConfigMaps are present

### 3. Run a Test Upgrade

```bash
kubectl apply -f test-upgrade.yaml
```

### 4. Monitor the Upgrade

```bash
# Watch the upgrade progress
kubectl get liqoupgrades.upgrade.liqo.io -A -w

# Check detailed status
kubectl describe liqoupgrades.upgrade.liqo.io liqo-upgrade-test -n liqo

# View operator logs
kubectl logs -n liqo-upgrade-controller-system deployment/liqo-upgrade-controller-controller-manager -f
```

## Example Files

### test-upgrade.yaml

A basic LiqoUpgrade resource that upgrades from v1.0.0 to v1.0.1.

```yaml
apiVersion: upgrade.liqo.io/v1alpha1
kind: LiqoUpgrade
metadata:
  name: liqo-upgrade-test
  namespace: liqo
spec:
  targetVersion: v1.0.1
  namespace: liqo
```

## Troubleshooting

### ConfigMap 'liqo-cluster-id' does not exist

If you see this error, run the setup script:

```bash
./setup-test-environment.sh
```

The script will automatically create the missing ConfigMap.

### Phase Cycling Between Validating and UpgradingCRDs

This was a bug that has been fixed. Ensure you're running the latest version of the operator. If the issue persists:

1. Delete the existing upgrade resource
2. Rebuild and redeploy the operator
3. Run the setup script again
4. Create a new upgrade resource

### Upgrade Fails During Prerequisites Validation

The Stage 2 validation checks that all ConfigMaps and Secrets referenced by the target version exist. Common causes:

1. **Missing liqo-cluster-id ConfigMap**: Run `./setup-test-environment.sh`
2. **Missing compatibility ConfigMap**: Apply `config/default/compatibility-configmap.yaml`
3. **Missing target descriptors**: Apply `config/default/target-descriptors-configmap.yaml`

### Checking Job Logs

If an upgrade stage fails, check the logs of the corresponding job:

```bash
# CRD upgrade job
kubectl logs -n liqo job/liqo-upgrade-crd-<upgrade-name>

# Controller manager upgrade job
kubectl logs -n liqo job/liqo-upgrade-controller-manager-<upgrade-name>

# Network fabric upgrade job
kubectl logs -n liqo job/liqo-upgrade-network-fabric-<upgrade-name>

# Rollback job (if upgrade failed)
kubectl logs -n liqo job/liqo-rollback-<upgrade-name>
```

## Cleanup

To clean up test resources:

```bash
# Delete the upgrade resource
kubectl delete -f test-upgrade.yaml

# Delete generated ConfigMaps (optional)
kubectl delete configmap liqo-upgrade-plan-liqo-upgrade-test -n liqo
kubectl delete configmap liqo-snapshot-liqo-upgrade-test -n liqo

# Uninstall the operator (optional)
make undeploy IMG=<your-registry>/liqo-upgrade-controller:<tag>
```

## Advanced Testing

### Testing with Custom Versions

Modify the target descriptors ConfigMap to add custom version descriptors:

```bash
kubectl edit configmap liqo-target-descriptors -n liqo
```

Then update `test-upgrade.yaml` to target your custom version.

### Testing Rollback

To test the rollback functionality, you can intentionally cause an upgrade to fail:

1. Modify the target descriptor to reference a non-existent image
2. Apply the upgrade
3. Watch as the upgrade fails and rolls back automatically

### Testing with Multiple Clusters

If you have Liqo peered clusters, the operator will automatically:
1. Detect all remote cluster versions from ForeignCluster CRs
2. Use the minimum version across all clusters for compatibility checking
3. Ensure the upgrade is compatible with all clusters

## Support

For issues or questions:
- Check the [main README](../README.md)
- View operator logs for detailed error messages
- Use the diagnostic script: `kubectl apply -f config/debug/diagnostic-job.yaml`
