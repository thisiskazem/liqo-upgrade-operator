# Liqo Upgrade Operator

A Kubernetes operator for **seamless, minimal-downtime upgrades** of Liqo multi-cluster deployments.

## Overview

The Liqo Upgrade Operator automates the complex process of upgrading Liqo across peered Kubernetes clusters. It handles:

- **CRD upgrades** (Stage 1) - Updates Custom Resource Definitions with proper migration
- **Control plane upgrades** (Stage 2) - Upgrades controller-manager, webhook, telemetry, and other core components
- **Network & data plane upgrades** (Stage 3) - Upgrades fabric, IPAM, gateways, and Virtual Kubelets with HA mode for minimum-downtime
- **Automatic rollback** - Reverts all changes if any stage fails
- **Configuration management** - Handles image versions, environment variables and arguments

### How It Works

1. Create a `LiqoUpgrade` custom resource specifying the target version
2. The operator compares current vs target version configurations
3. Executes a multi-stage upgrade with health checks at each step
4. Maintains cluster connectivity throughout the process using gateway HA mode
5. Automatically rolls back on failure to ensure cluster stability

---

## Prerequisites

- Kubernetes cluster (v1.26+)
- Liqo installed (v1.0.0+)
- `kubectl` configured
- `helm` v3.x (for Helm-based installation)

---

## Installation

### Option 1: Argo CD (GitOps)

```
kubectl apply -f https://raw.githubusercontent.com/thisiskazem/liqo-upgrade-operator/main/examples/argocd-application.yaml 
```

### Option 2: Helm (Public Repository)

#### Add the Helm repository

```
helm repo add liqo-upgrade https://thisiskazem.github.io/liqo-upgrade-operator
helm repo update
```

#### Install the operator

```
helm install liqo-upgrade-operator liqo-upgrade/liqo-upgrade-operator \
  -n liqo \
  --create-namespace
```

#### Verify installation

```
kubectl get pods -n liqo -l app.kubernetes.io/name=liqo-upgrade-operator
```

### Option 3: Helm (Local Clone)

#### Clone the repository

```
git clone https://github.com/thisiskazem/liqo-upgrade-operator.git
cd liqo-upgrade-operator
```

#### Install from local chart

```
helm install liqo-upgrade-operator ./chart/liqo-upgrade-operator \
  -n liqo \
  --create-namespace
```

### Option 4: Kustomize

#### Clone the repository

```
git clone https://github.com/thisiskazem/liqo-upgrade-operator.git
cd liqo-upgrade-operator
```

#### Build and push the operator image (optional, if using custom image)

```
make docker-build docker-push IMG=<your-registry>/liqo-upgrade-operator:latest
```

#### Deploy using Kustomize

```
make deploy IMG=<your-registry>/liqo-upgrade-operator:latest
```

## Usage

### Apply the LiqoUpgrade CR

```
kubectl apply -f examples/upgrade.yaml  
```

### Watch upgrade status

```
kubectl get liqoupgrade -n liqo -w
```

### View detailed status

```
kubectl describe liqoupgrade <CR name> -n liqo
```

### Check upgrade job logs

```
kubectl logs -n liqo -l job-name=liqo-upgrade-controller-manager-<CR name> -f
```

## Upgrade Stages

| Stage | Description | Components |
|-------|-------------|------------|
| 1 | CRD Upgrade | Custom Resource Definitions |
| 2 | Control Plane | controller-manager, webhook, crd-replicator, metric-agent, telemetry |
| 3 | Network & Data Plane | fabric, IPAM, proxy, gateways, Virtual Kubelets |

---

## Configuration

### Helm Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Operator image repository | `ghcr.io/liqotech/liqo-upgrade-operator` |
| `image.tag` | Operator image tag | `latest` |
| `namespace` | Namespace for operator | `liqo` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |
| `metrics.enabled` | Enable metrics service | `true` |

### Example with custom values

```
helm install liqo-upgrade-operator liqo-upgrade/liqo-upgrade-operator \
  -n liqo \
  --set image.tag=v0.1.0 \
  --set resources.limits.memory=256Mi---
```

## Uninstallation

### Helm

```
helm uninstall liqo-upgrade-operator -n liqo### Kustomize
make undeploy---
```

## Development

### Build

```
make build
```

### Run Locally

```
make run
```

### Run Tests

```
make test
```

### Lint

```
make lint---
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      LiqoUpgrade CR                         │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                   Liqo Upgrade Operator                     │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐          │
│  │   Stage 1   │→ │   Stage 2   │→ │   Stage 3   │          │
│  │   CRD Job   │  │ CtrlMgr Job │  │ Network Job │          │
│  └─────────────┘  └─────────────┘  └─────────────┘          │
└─────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
        ┌──────────┐   ┌──────────┐   ┌──────────┐
        │   CRDs   │   │  Control │   │ Network  │
        │          │   │  Plane   │   │  Fabric  │
        └──────────┘   └──────────┘   └──────────┘
```

## Contributing

1. Fork the repository
2. Create a feature branch

```
git checkout -b feature/amazing-feature
```

3. Commit your changes

```
git commit -m 'feat: add amazing feature'
```

4. Push to the branch

```
git push origin feature/amazing-feature
```

5. Open a Pull Request


## License

This project is licensed under the Apache 2.0 License.


## Links

- **Helm Repository**: [thisiskazem.github.io/liqo-upgrade-operator](https://thisiskazem.github.io/liqo-upgrade-operator)
- **Artifact Hub**: [artifacthub.io/packages/helm/liqo-upgrade-operator](https://artifacthub.io/packages/helm/liqo-upgrade-operator/liqo-upgrade-operator)
- **Liqo Project**: [liqo.io](https://liqo.io)