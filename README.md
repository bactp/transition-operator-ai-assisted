# AI-Assisted Transition Operator

**AI 에이전트 지원 Kubernetes 멀티클러스터 워크로드 전환(페일오버) 오케스트레이션 프로그램**

AI-agent-assisted failure detection and safe workload failover (transition) across multiple
Kubernetes clusters, with human-in-the-loop approval, for cloud-native and multi-cluster environments.

> For the full, step-by-step environment setup (cloud providers, CNI, CRIU/runc, fault injection,
> measurement), see the [User Guide](USER-GUIDE.md). The operator's recovery logic is described in
> [recovery-flow.md](recovery-flow.md).

---

## Overview

The operator runs on a **management cluster** and watches **workload clusters** created with
Cluster API. When a workload cluster fails, an **AI agent recommends where to fail over**, a
**human approves** the recommendation, and the operator then transitions the running workloads
(stateless and stateful) to the healthy recovery-target cluster.

- **AI-assisted failover** – an LLM-based agent ([kagent](https://kagent.dev)) analyses the fault
  and recommends a failover target through a purpose-built MCP server (`failover-agent-mcp`),
  instead of accessing the Kubernetes API directly.
- **Human-in-the-loop approval** – the agent's recommendation and the human approval are recorded
  as durable control-plane state (`ClusterPolicy` status phases). The deterministic operator
  remains the **only** component that executes transitions.
- **Policy-driven transition** – the `ClusterPolicy` CRD declares the source cluster, the
  application packages, backup/checkpoint settings, and the preferred target clusters.
- **Stateful migration** – running containers are checkpointed with CRIU, stored as checkpoint
  images in a container registry and in MinIO/S3, and restored on the target cluster.
- **Bounded agent actions** – the AI agent's action surface is restricted by three independent
  boundaries:
  1. an agent tool allowlist (`toolNames` in the kagent `Agent`),
  2. scoped RBAC for the MCP server (read-only on CRDs, write only to `clusterpolicies/status`),
  3. operator-side validation and approval semantics (out-of-policy targets are rejected; nothing
     executes until a human sets the phase to `Approved`).

### Main functions

| Area | Function |
|------|----------|
| CRDs | Manage transition policies, checkpoints, node health and package policies with `ClusterPolicy`, `Checkpoint`, `NodeHealth` and `PackagePolicy`. |
| Fault detection | Monitor workload cluster health via heartbeat, control-plane reachability and CNI status checks; detect faults automatically. |
| Checkpoint / restore | Checkpoint running containers, push checkpoint images to a registry and MinIO/S3, and restore workloads on the target cluster. |
| Package redeploy | Redeploy application packages on the target cluster through Git-based (Gitea/Nephio) package management. |
| MCP tools | `get_cluster_health`, `get_checkpoint_status`, `get_transition_candidates`, `propose_failover`. |
| Recommendation | Record the AI recommendation (target cluster, reason, recommender) in `ClusterPolicy` status and block execution while the phase is `AwaitingApproval`. |
| Approval | Execute the transition only after human approval (`Approved → Executing → Completed`); recommendations can be rejected (`Rejected`). |
| Backward compatibility | `ClusterPolicy` objects that are not opted into AI-assisted mode keep the fully automatic transition behavior. |

### Architecture

```text
                    ┌──────────────────────── Management cluster ────────────────────────┐
                    │                                                                    │
  Human operator ──►│  kubectl patch status.phase=Approved / Rejected                    │
                    │            │                                                       │
                    │            ▼                                                       │
                    │   ┌──────────────────┐   status: AwaitingApproval   ┌────────────┐ │
                    │   │  ClusterPolicy   │◄─────── propose_failover ────│ failover-  │ │
                    │   │  (CRD + status)  │                              │ agent-mcp  │ │
                    │   └────────┬─────────┘   get_* (read-only tools)    │ (MCP srv)  │ │
                    │            │            ─────────────────────────►  └─────▲──────┘ │
                    │            ▼                                              │        │
                    │   ┌──────────────────┐      kagent invoke          ┌──────┴──────┐ │
                    │   │ transition-      │ ──────────────────────────► │ kagent Agent│ │
                    │   │ operator         │                             │ + LLM       │ │
                    │   └──┬──────┬─────┬──┘◄── heartbeat :8090          └─────────────┘ │
                    └──────┼──────┼─────┼───────(NodePort 30092)─────────────────────────┘
                           │      │     │              ▲
                 Gitea/Nephio  MinIO/S3  Registry      │
                           │      │     │              │
                    ┌──────▼──────┴─────▼──┐        ┌──┴───────────────────┐
                    │ Target workload      │        │ Source workload      │
                    │ cluster (restore)    │        │ cluster + checkpoint │
                    └──────────────────────┘        │ agent (DaemonSet)    │
                                                    └──────────────────────┘
```

Failover flow:

1. The operator detects a fault (heartbeat, control-plane or CNI) on the source cluster and
   blocks the transition.
2. It calls the `failover-agent` through the `kagent invoke` CLI (bundled in the operator image).
3. The agent inspects the situation with the read-only MCP tools and, if a failover is warranted,
   calls `propose_failover`, which writes the recommendation into the `ClusterPolicy` status
   (`AwaitingApproval`).
4. A human reviews the recommendation and patches the phase to `Approved` (or `Rejected`).
5. The operator checkpoints, restores and redeploys the workloads on the target cluster.

`ClusterPolicy` objects without the `ai-assisted` annotation keep the legacy fully automatic
behavior (step 1 goes straight to step 5).

---

## Requirements

| Item | Value |
|------|-------|
| Operating system | Ubuntu |
| Language | Go (>= 1.24) |
| Kubernetes | >= 1.30 on all clusters, managed with Cluster API (`clusterctl`) |
| Tooling | Go, Docker, kubectl, Kustomize, Helm, clusterctl |
| Node runtime | containerd >= 2.1.4, CRIU >= 4.1.1, runc — **identical versions on source and target nodes** |
| Services | Gitea (Git server), MinIO or S3-compatible object storage, container registry |
| AI agent | kagent (with the kMCP `MCPServer` CRD), an LLM provider (OpenAI by default) |

---

## Getting Started

The steps below summarize how to run the software. Details for each step are in the
[User Guide](USER-GUIDE.md).

### 1. Prepare the clusters

1. Create a management cluster and the workload clusters with Cluster API
   ([User Guide §3–5](USER-GUIDE.md#5-management-cluster-setup)).
2. Install Go, Docker, kubectl, Helm and clusterctl on the management cluster.
3. On every source and target worker node, install CRIU, align the `runc` version, create
   `/etc/criu/runc.conf`, and enable the kubelet `ContainerCheckpoint` feature gate
   ([User Guide §6](USER-GUIDE.md#6-prepare-worker-nodes-source-and-destination)):

   ```sh
   # /etc/default/kubelet
   KUBELET_EXTRA_ARGS="--feature-gates=ContainerCheckpoint=true"
   ```

   ```sh
   systemctl restart kubelet containerd
   ```

### 2. Configure secrets and environment variables (management cluster)

The operator reads its configuration from environment variables. When deployed with
`make deploy`, [config/manager/manager.yaml](config/manager/manager.yaml) fills them from these
secrets in the operator namespace (`transition-operator-system`):

| Secret | Keys |
|--------|------|
| `minio-secret` | `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY`, `MINIO_BUCKET` |
| `reg-credentials` | `username`, `password` (Docker Hub access token), `registry` |
| `git-secret` | `GIT_SERVER_URL`, `GIT_SECRET_NAME`, `GIT_SECRET_NAMESPACE` |

The operator also looks up `reg-credentials` and the Gitea login secret (`GIT_SECRET_NAME`, e.g.
`git-user-secret`) in the `default` namespace:

```sh
kubectl create namespace transition-operator-system

for ns in transition-operator-system default; do
  kubectl create secret generic reg-credentials -n $ns \
    --from-literal=username=<registry-user> \
    --from-literal=password=<registry-token> \
    --from-literal=registry=docker.io
done

kubectl create secret generic minio-secret -n transition-operator-system \
  --from-literal=MINIO_ENDPOINT=<minio-host>:<port> \
  --from-literal=MINIO_ACCESS_KEY=<access-key> \
  --from-literal=MINIO_SECRET_KEY=<secret-key> \
  --from-literal=MINIO_BUCKET=checkpoints

kubectl create secret generic git-secret -n transition-operator-system \
  --from-literal=GIT_SERVER_URL=http://<gitea-host>:<port> \
  --from-literal=GIT_SECRET_NAME=git-user-secret \
  --from-literal=GIT_SECRET_NAMESPACE=default

kubectl create secret generic git-user-secret -n default \
  --from-literal=username=<git-user> \
  --from-literal=password=<git-password>
```

> Templates live in [config/secrets/](config/secrets). Files with real credentials in that folder
> are git-ignored — never commit them. `config/secrets` is not part of the kustomize build; apply
> it with `kubectl apply -f`.

When running the operator locally (`make run`), export the same values instead:

```sh
export MINIO_ENDPOINT="<minio-host>:<port>"
export MINIO_ACCESS_KEY="<access-key>"
export MINIO_SECRET_KEY="<secret-key>"
export MINIO_BUCKET="checkpoints"

export REPOSITORY="<registry-user>"
export REGISTRY_PASSWORD="<registry-token>"
export REGISTRY_URL="docker.io"
export SECRET_NAME_REF="reg-credentials"
export SECRET_NAMESPACE_REF="default"

export GIT_SERVER_URL="http://<gitea-host>:<port>"
export GIT_SECRET_NAME="git-user-secret"
export GIT_SECRET_NAMESPACE="default"
export POD_NAMESPACE="default"
export HEARTBEAT_FAULT_DELAY=20

# AI agent (leaving KAGENT_AGENT_NAME unset disables agent notification)
export KAGENT_AGENT_NAME="failover-agent"
export KAGENT_AGENT_NAMESPACE="kagent"
export KAGENT_URL="http://<kagent-controller>:8083"
```

### 3. Generate code and install the CRDs

```sh
make generate
make manifests
make install
```

### 4. Build and deploy the operator

Build, push and deploy it into the management cluster:

```sh
make docker-build docker-push IMG=<registry>/transition-operator:<tag>
make deploy IMG=<registry>/transition-operator:<tag>
```

Or run it locally against the current kubeconfig (requires the `kagent` CLI on `PATH`):

```sh
make run
```

> The image includes buildah and the `kagent` CLI, and the manager runs privileged (rootless
> buildah cannot commit checkpoint images otherwise). The manager Deployment is pinned to one node
> with `nodeSelector` and exposes the heartbeat endpoint as NodePort `30092`
> ([config/default/metrics_service.yaml](config/default/metrics_service.yaml)). Adjust the node
> name in [config/manager/manager.yaml](config/manager/manager.yaml) to match your cluster.
> If you hit RBAC errors, make sure you are logged in with cluster-admin privileges.

### 5. Deploy the AI failover agent (kagent + MCP server)

Prerequisites: kagent installed in the `kagent` namespace (with the kMCP `MCPServer` CRD) and a
secret holding the LLM API key:

```sh
kubectl create secret generic kagent-openai -n kagent --from-literal=OPENAI_API_KEY=<api-key>
```

1. Build the `failover-agent-mcp` image. Run this from the **repository root**, because the MCP
   server imports `api/` and `reconcilers/`:

   ```sh
   docker build -f mcp/failover-agent-mcp/Dockerfile -t failover-agent-mcp:demo .
   ```

   [mcpserver.yaml](mcp/failover-agent-mcp/manifests/mcpserver.yaml) uses
   `imagePullPolicy: Never` and a `nodeSelector`, so load the image onto that node (for example
   `docker save failover-agent-mcp:demo | ctr -n k8s.io images import -`), or push it to a
   registry and update the image fields.

2. Deploy the MCP server, its RBAC, the model config and the kagent `Agent`:

   ```sh
   kubectl apply -f mcp/failover-agent-mcp/manifests/rbac.yaml
   kubectl apply -f mcp/failover-agent-mcp/manifests/mcpserver.yaml
   kubectl apply -f mcp/failover-agent-mcp/manifests/modelconfig.yaml
   kubectl apply -f mcp/failover-agent-mcp/manifests/agent.yaml
   ```

3. Check that the agent and the MCP server are ready:

   ```sh
   kubectl get agents,mcpservers -n kagent
   ```

The MCP server exposes only these tools to the agent:

| Tool | Purpose |
|------|---------|
| `get_cluster_health` | Read workload cluster health (CAPI cluster and machines) |
| `get_checkpoint_status` | Read checkpoint state for the packages |
| `get_transition_candidates` | List the policy's preferred target clusters with live health and checkpoint image availability |
| `propose_failover` | Write a recommendation (target, reason, recommender) into the `ClusterPolicy` status; never executes it |

See [mcp/failover-agent-mcp/README.md](mcp/failover-agent-mcp/README.md) for local development of
the MCP server.

### 6. Run the checkpoint agent on the source cluster

On the **source** workload cluster, apply the checkpoint service account and the checkpoint agent
DaemonSet. Edit `MINIO_ENDPOINT`, `CONTROLLER_URL` (`http://<mgmt-node-ip>:30092/heartbeat`) and
the `minio-credentials` secret values first:

```sh
kubectl apply -f deploy/checkpoint-sa-rbac.yaml --kubeconfig <source-cluster-kubeconfig>
kubectl apply -f deploy/checkpoint-agent-daemonset.yaml --kubeconfig <source-cluster-kubeconfig>
```

Alternatively, run the [checkpoint agent](https://github.com/vitu-mafeni/checkpoint-agent) by
hand on the source worker node ([User Guide §10](USER-GUIDE.md#10-workload-cluster-source-worker-node)).

### 7. Apply an AI-assisted ClusterPolicy

Apply a `ClusterPolicy` on the management cluster that specifies the source cluster, the
packages and the preferred target clusters, with the
`transition.dcnlab.ssu.ac.kr/approval-mode: ai-assisted` annotation. A ready-made example is
[clusterpolicy-demo.yaml](mcp/failover-agent-mcp/manifests/clusterpolicy-demo.yaml):

```yaml
apiVersion: transition.dcnlab.ssu.ac.kr/v1
kind: ClusterPolicy
metadata:
  name: clusterpolicy-sample
  namespace: default
  annotations:
    transition.dcnlab.ssu.ac.kr/approval-mode: ai-assisted
spec:
  clusterSelector:
    name: cluster1-azure
    repo: http://<gitea-host>:<port>/nephio/cluster1-azure.git
    repoType: git
  packageSelectors:
    - name: video
      packagePath: video
      packageType: Stateful        # Stateless | Stateful
      liveStatePackage: true
      backupInformation:
        - name: my-test-backup
          backupType: Schedule     # Manual | Schedule
          schedulePeriod: "*/2 * * * *"
  targetClusterPolicy:
    preferClusters:
      - name: cluster2-aws
        repoType: git
        weight: 100
```

```sh
kubectl apply -f <your-clusterpolicy>.yaml
```

> The samples in [config/samples](config/samples) have no annotation, so they run in the legacy
> fully automatic mode. Add the annotation to use them with the AI agent:
> `kubectl annotate clusterpolicy <name> -n default transition.dcnlab.ssu.ac.kr/approval-mode=ai-assisted`

### 8. Review and approve the recommendation

When a fault is detected, the agent records its recommendation and the `ClusterPolicy` moves to
`AwaitingApproval`; the operator does not transition anything in this phase.

```sh
kubectl get clusterpolicy clusterpolicy-sample -n default -o jsonpath='{.status.phase}'
kubectl get clusterpolicy clusterpolicy-sample -n default -o jsonpath='{.status.recommendation}'
```

Approve the recommendation so the operator performs the transition:

```sh
kubectl patch clusterpolicy clusterpolicy-sample -n default --subresource=status --type=merge \
  -p '{"status":{"phase":"Approved"}}'
```

Or reject it:

```sh
kubectl patch clusterpolicy clusterpolicy-sample -n default --subresource=status --type=merge \
  -p '{"status":{"phase":"Rejected"}}'
```

Phase lifecycle:

```text
(none) ──► AwaitingApproval ──► Approved ──► Executing ──► Completed
                  │
                  └──────────► Rejected
```

- `propose_failover` sets `Rejected` itself if the agent proposes a target that is not in
  `spec.targetClusterPolicy.preferClusters`.
- The operator is allowed to transition only in `Approved`, `Executing` or `Completed`. Progress of
  each package is reported in `status.transitionedPackages`.

---

## Testing and fault injection

- CNI fault, API-server fault and recovery procedures: [User Guide – Fault Injection](USER-GUIDE.md#fault-injection).
- Recovery-time measurement with [test-scripts/measurement.py](test-scripts/measurement.py) and
  [apps-config.yaml](apps-config.yaml):

  ```sh
  python3 test-scripts/measurement.py --apps-config apps-config.yaml
  ```

- Unit tests and e2e tests (e2e requires Kind):

  ```sh
  make test
  make test-e2e
  ```

---

## Uninstall

```sh
kubectl delete -f mcp/failover-agent-mcp/manifests/
kubectl delete clusterpolicy --all -n default                 # delete your CRs
make undeploy
make uninstall
```

---

## Project layout

| Path | Contents |
|------|----------|
| `api/v1/` | CRD types: `ClusterPolicy` (incl. approval phases and recommendation), `Checkpoint`, `NodeHealth`, `PackagePolicy` |
| `internal/controller/` | Controllers for each CRD, including the AI-assisted approval gate |
| `reconcilers/` | Transition logic: CAPI, checkpoint/restore, heartbeat, control-plane and CNI fault detection, Gitea and MinIO clients |
| `config/` | Kustomize manifests (CRDs, RBAC, manager), secret templates and samples |
| `deploy/` | Checkpoint agent DaemonSet and checkpoint RBAC for the source workload cluster |
| `mcp/failover-agent-mcp/` | MCP server (Go, kmcp) and kagent `Agent`, `ModelConfig`, `MCPServer` and RBAC manifests |
| `test-scripts/` | Fault injection and measurement scripts |

---

## License

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
