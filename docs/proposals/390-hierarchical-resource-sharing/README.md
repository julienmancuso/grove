# Hierarchical Resource Sharing

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
    - [The Need for Multiple Sharing Scopes](#the-need-for-multiple-sharing-scopes)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
- [Proposal](#proposal)
    - [User Stories (*Optional*)](#user-stories-optional)
        - [Story 1: Resilient Inference with Per-Replica GPU Sharing](#story-1-resilient-inference-with-per-replica-gpu-sharing)
        - [Story 2: Multi-Stage Training Pipeline with GPU Sharing](#story-2-multi-stage-training-pipeline-with-gpu-sharing)
    - [Limitations/Risks &amp; Mitigations](#limitationsrisks--mitigations)
- [Design Details](#design-details)
    - [ResourceClaim Naming Convention](#resourceclaim-naming-convention)
    - [Owner References](#owner-references)
    - [Common Types](#common-types)
    - [PodClique-Level Resource Sharing](#podclique-level-resource-sharing)
    - [PodCliqueScalingGroup-Level Resource Sharing](#podcliquescalinggroup-level-resource-sharing)
    - [Multiple Pods Per Replica](#multiple-pods-per-replica)
    - [Per-Replica Resource Sharing](#per-replica-resource-sharing)
    - [Monitoring](#monitoring)
    - [Dependencies (*Optional*)](#dependencies-optional)
    - [Test Plan](#test-plan)
    - [Graduation Criteria](#graduation-criteria)
- [Implementation History (*Optional*)](#implementation-history-optional)
- [Alternatives (*Optional*)](#alternatives-optional)
- [Appendix (*Optional*)](#appendix-optional)

<!-- /toc -->

## Summary

Grove provides a hierarchical and flexible Kubernetes API to describe inference and training workloads. It encodes in 
scheduling and scaling constraints at every level of a `PodCliqueSet` (PCS). A PCS can directly contain one 
or more `PodClique` (PCLQ) instances and/or one or more `PodCliqueScalingGroup` (PCSG) instances, where each PCSG in 
turn contains one or more PCLQ instances.

This GREP enhances the `PodCliqueSet` API to allow sharing of cluster resources (such as GPU accelerators) amongst a 
group of pods at multiple levels of the Grove hierarchy by leveraging [ResourceClaim](https://github.com/kubernetes/api/blob/ffebe2b51dedadf6a36343b495ca26060cb7a93d/resource/v1/types.go#L741)
offered via Dynamic Resource Allocation (DRA) in Kubernetes. Users provide inline `ResourceClaimTemplateSpec`
definitions via a unified `ResourceAllocation` type with a `Scope` enum (`PerInstance` or `PerReplica`), and Grove
creates and manages `ResourceClaim` objects directly — no `ResourceClaimTemplate` objects are created. The design
enables:

* All pods within a PCLQ instance sharing resources (`PerInstance` scope at PCLQ level),
* All pods within a single PCLQ replica slot sharing resources (`PerReplica` scope at PCLQ level),
* All pods across a subset of PCLQs within a PCSG replica sharing resources (`PerReplica` scope at PCSG level), or
* All pods across all replicas of a PCSG sharing resources (`PerInstance` scope at PCSG level),

while ensuring proper isolation between replicas during scaling operations. Additionally, a `PodsPerReplica` field
on `PodCliqueSpec` allows creating multiple pods per replica slot (e.g. for crash resilience via shadow pods),
orthogonal to resource allocation.

## Motivation

Modern ML inference and training workloads often require multiple pods to share expensive cluster resources such as GPU 
accelerators to optimize resource utilization and reduce costs. Grove's hierarchical API (PCS → PCSG → PCLQ) provides 
natural boundaries for defining resource sharing scopes, but currently lacks the ability to specify how resources should 
be shared within these boundaries.

Kubernetes DRA provides `ResourceClaim` and `ResourceClaimTemplate` APIs that enable resource sharing, but using them 
directly in Grove's pod templates presents challenges:

- **ResourceClaim in pod templates**: All pods created from the template reference the same claim, which breaks isolation 
  when PodCliques are instantiated multiple times across PCSG or PCS replicas.
- **ResourceClaimTemplate in pod templates**: Each pod gets a unique ResourceClaim, preventing any sharing within the 
  desired scope (PCLQ or PCSG).

Grove needs a mechanism to orchestrate resource sharing that respects its hierarchical structure—allowing resources to 
be shared within a PCLQ instance or across a subset of PCLQs within a PCSG instance, while maintaining proper isolation 
across different instances during scaling operations.

### The Need for Multiple Sharing Scopes

Real-world workloads require resource sharing at different granularities within the Grove hierarchy. Consider a
disaggregated inference workload with multiple pods per replica for crash resilience:

- **PCSG `PerReplica`**: An NVSwitch or interconnect resource shared across all PodCliques in a scaling group
  replica (e.g. a leader and its workers sharing a fabric).
- **PCSG `PerInstance`**: A shared storage or interconnect resource shared across ALL replicas of a scaling group.
- **PCLQ `PerInstance`**: A set of GPUs shared across all replicas of a PodClique instance (e.g. all worker
  replicas in a scaling group replica share one pool of GPUs).
- **PCLQ `PerReplica`**: A GPU partition shared between all pods within a single replica slot, enabling
  zero-downtime recovery without resource reallocation.

These scopes are orthogonal and composable. A single PodClique may participate in multiple scopes simultaneously.
Without per-replica sharing, extra pods in a replica slot cannot share hardware with the active pod without also
sharing with every other replica — defeating the isolation needed for independent recovery.

The `PodsPerReplica` field (number of pods per replica slot) is orthogonal to resource allocation. You can have
multiple pods per replica without any resource sharing, or per-replica resource sharing with a single pod per
replica.

### Goals

- Enable users to define resource sharing primitives at multiple levels of Grove hierarchy (PodClique and
  PodCliqueScalingGroup) via a unified `ResourceAllocation` type with a `Scope` enum.
- Users should be able to limit and scope resource sharing within a subset of a group or within a specific level,
  e.g. share resources between pods of a PodClique instance (`PerInstance`) vs between pods of a single replica
  (`PerReplica`), or between a subset of PCLQs within a PCSG instance.
- Enable users to provide inline `ResourceClaimTemplateSpec` definitions for resource sharing groups.
- Enable per-replica resource sharing within a PodClique so that pods within a replica slot can share resources
  while maintaining isolation from other replicas.
- Enable configuring multiple pods per replica slot (`PodsPerReplica`) orthogonally from resource allocation.

### Non Goals

_(none at this time)_

## Proposal




### User Stories (*Optional*)

#### Story 1: Resilient Inference with Per-Replica GPU Sharing

A platform team deploys a disaggregated inference workload with a prefill leader (PCA, 3 replicas) and prefill workers
(PCB, 2 replicas). Each replica has `podsPerReplica: 2` for crash resilience. All pods in a replica slot are
identical — the application uses lease-based election to determine which pod is active. The standby pod holds references
to the same GPU memory so it can take over instantly without reloading model weights.

The workload requires three levels of resource sharing:

1. **PCSG `PerReplica`**: An NVSwitch fabric claim shared across all pods in the scaling group replica
   (leader + workers).
2. **PCLQ `PerInstance`**: A GPU pool claim shared across all replicas of a PodClique instance — e.g. all 3
   PCA replicas and their standby pods share one set of GPUs.
3. **PCLQ `PerReplica`**: A GPU partition claim shared only between all pods within a single replica slot —
   enabling each replica to recover independently.

_Challenge_: Without per-replica sharing, a standby pod would either get its own exclusive GPU allocation (wasting
resources) or share with all replicas (breaking isolation). The `PerReplica` scope fills this gap.

_Solution_: Grove orchestrates resource sharing via a unified `ResourceAllocation` type with scope enum:

- `resourceAllocations` at the PCSG level with `scope: PerReplica` creates one ResourceClaim per PCSG replica,
  injected into all PodCliques in that replica.
- `resourceAllocations` at the PCLQ level with `scope: PerInstance` creates one ResourceClaim per PCLQ instance,
  shared across all replicas and all pods in each replica slot.
- `resourceAllocations` at the PCLQ level with `scope: PerReplica` creates one ResourceClaim per replica slot,
  shared between all pods in that slot.

**Concrete example** of the ResourceClaim distribution:

```
PCS:
  cliques:
    - PCA: replicas=3, podsPerReplica=2,
           resourceAllocations=[{scope: PerInstance, spec: RCT-N}, {scope: PerReplica, spec: RCT-SHD}]
    - PCB: replicas=2, podsPerReplica=2,
           resourceAllocations=[{scope: PerInstance, spec: RCT-P}, {scope: PerReplica, spec: RCT-SHD}]
  scalingGroups:
    - SGX: {PCA, PCB}, resourceAllocations=[{scope: PerReplica, spec: RCT-M, cliqueNames: [PCA, PCB]}]

SGX-0: RC-M0   (PCSG PerReplica — shared by ALL pods in SGX-0)
  SGX-0-PCA: RC-N0   (PCLQ PerInstance — shared by all 6 pods in PCA)
    {SGX-0-PCA-0-pod-0, SGX-0-PCA-0-pod-1} → RC-SHD-SGX-0-PCA-0   (PCLQ PerReplica)
    {SGX-0-PCA-1-pod-0, SGX-0-PCA-1-pod-1} → RC-SHD-SGX-0-PCA-1   (PCLQ PerReplica)
    {SGX-0-PCA-2-pod-0, SGX-0-PCA-2-pod-1} → RC-SHD-SGX-0-PCA-2   (PCLQ PerReplica)
  SGX-0-PCB: RC-P0   (PCLQ PerInstance — shared by all 4 pods in PCB)
    {SGX-0-PCB-0-pod-0, SGX-0-PCB-0-pod-1} → RC-SHD-SGX-0-PCB-0   (PCLQ PerReplica)
    {SGX-0-PCB-1-pod-0, SGX-0-PCB-1-pod-1} → RC-SHD-SGX-0-PCB-1   (PCLQ PerReplica)

SGX-1: RC-M1
  SGX-1-PCA: RC-N1
    {SGX-1-PCA-0-pod-0, SGX-1-PCA-0-pod-1} → RC-SHD-SGX-1-PCA-0
    {SGX-1-PCA-1-pod-0, SGX-1-PCA-1-pod-1} → RC-SHD-SGX-1-PCA-1
    {SGX-1-PCA-2-pod-0, SGX-1-PCA-2-pod-1} → RC-SHD-SGX-1-PCA-2
  SGX-1-PCB: RC-P1
    {SGX-1-PCB-0-pod-0, SGX-1-PCB-0-pod-1} → RC-SHD-SGX-1-PCB-0
    {SGX-1-PCB-1-pod-0, SGX-1-PCB-1-pod-1} → RC-SHD-SGX-1-PCB-1
```

In this example:
- Pod naming: `{PCLQ-name}-{replica-index}-pod-{pod-index}` — all pods use the `-pod-` delimiter when `podsPerReplica > 1`
- RC-M0/RC-M1 are PCSG `PerReplica` claims: one per PCSG replica, shared by every pod in that replica
- RC-N0/RC-P0 are PCLQ `PerInstance` claims: one per PCLQ instance, shared by all replicas and pods
- RC-SHD-* are PCLQ `PerReplica` claims: one per replica slot, shared between all pods in that slot

#### Story 2: Multi-Stage Training Pipeline with GPU Sharing

Multi-stage ML pipelines with separate preprocessing and training components are a common pattern in production ML systems. Frameworks like [Kubeflow Pipelines](https://www.kubeflow.org/docs/components/pipelines/v1/introduction/), [TensorFlow Extended (TFX)](https://www.tensorflow.org/tfx), and [Ray Train](https://docs.ray.io/en/latest/train/train.html) enable users to define pipelines where data preprocessing (ETL, feature engineering, augmentation) runs as separate containers/pods from the training workload.

In such a distributed training pipeline, data preprocessing pods load and transform data into GPU memory, while model training pods consume this preprocessed data directly from GPU memory without expensive CPU-GPU transfers. Libraries like [NVIDIA DALI](https://docs.nvidia.com/deeplearning/dali/user-guide/docs/index.html) provide GPU-accelerated data preprocessing capabilities that make this pattern efficient. The preprocessing and training pods are modeled as separate PCLQs within a PCSG, where each PCSG replica represents a different training experiment.

_Challenge_: Each experiment (PCSG instance) needs its own isolated set of GPUs, but within an experiment, both preprocessing and training pods should share the same GPU devices for efficient data transfer and memory utilization. Standard GPU allocation creates exclusive claims per pod, preventing this sharing pattern. When these stages need to share GPUs for zero-copy data transfer and to avoid CPU-GPU memory copying overhead, DRA's [shareable ResourceClaims](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#shareable-resources) become essential.

_Solution_: By leveraging GPU sharing technologies like [NVIDIA Multi-Process Service (MPS)](https://docs.nvidia.com/deploy/mps/index.html) for efficient GPU sharing or [CUDA IPC (Inter-Process Communication)](https://docs.nvidia.com/cuda/cuda-c-programming-guide/index.html#interprocess-communication) for sharing GPU memory between processes, along with techniques like [GPU Direct Storage](https://developer.nvidia.com/gpudirect-storage) for direct data paths, Grove enables this pattern through `resourceAllocations` at the PCSG level. By specifying a `ResourceAllocation` with `scope: PerReplica` and `cliqueNames` referencing both the preprocessing and training PCLQs, Grove creates a ResourceClaim per PCSG replica that is shared across the specified PCLQs. This enables both pod types to access the same GPU devices within each experiment while maintaining isolation across different experiments.

### Limitations/Risks & Mitigations

<!-- 
What are the current set of limitations or risks of this proposal? Think broadly by considering the impact of the changes proposed on kubernetes ecosystem. Optionally mention ways to mitigate these.
-->

## Design Details

### ResourceClaim Naming Convention

Grove creates ResourceClaim objects directly from inline specs — no intermediate ResourceClaimTemplate objects are
created. This is because Kubernetes' built-in RCT-to-RC auto-creation (`resourceClaimTemplateName` in the pod spec)
creates a unique ResourceClaim per pod, which is the opposite of sharing. For shared claims, we must pre-create
ResourceClaim objects and reference them via `resourceClaimName` in the pod spec. Since the RCT would not participate
in any Kubernetes mechanism (no pod references it), it is omitted. The inline specs in each `ResourceAllocation`
entry serve as the source of truth. ResourceClaimTemplate objects can be added later for observability if needed.

Each RC name is derived from the owning resource's Kubernetes name plus a suffix that encodes the sharing
scope and the allocation index (position in the `ResourceAllocations` list).

| Level + Scope | RC Name Format |
|---|---|
| PCLQ `PerInstance` | `<pclq.Name>-rct-<allocIndex>` |
| PCLQ `PerReplica` | `<pclq.Name>-<replicaIndex>-rct-<allocIndex>` |
| PCSG `PerReplica` | `<pcsg.Name>-<pcsgReplicaIndex>-rct-<allocIndex>` |
| PCSG `PerInstance` | `<pcsg.Name>-rct-<allocIndex>` |

The `rct-<index>` suffix identifies the position of the entry in the `ResourceAllocations` list. For PCLQ
`PerReplica`, the replica index is inserted before the suffix to produce one RC per replica.

**Concrete example** — PCS `my-svc` (replica 0), PCSG `mi` (replicas: 2), cliques: `leader` (replicas: 2,
podsPerReplica: 2, PCLQ PerReplica alloc at index 0), `worker` (replicas: 3, PCLQ PerInstance alloc at index 0),
PCSG PerReplica alloc at index 0:

```
PCSG PerReplica ResourceClaims (owned by PCSG):
  my-svc-0-mi-0-rct-0        → shared by ALL pods in PCSG replica 0
  my-svc-0-mi-1-rct-0        → shared by ALL pods in PCSG replica 1

PCLQ PerInstance ResourceClaims (owned by PCLQ):
  my-svc-0-mi-0-worker-rct-0 → shared by all worker pods in PCSG replica 0
  my-svc-0-mi-1-worker-rct-0 → shared by all worker pods in PCSG replica 1

PCLQ PerReplica ResourceClaims (owned by PCLQ, explicit deletion on scale-down):
  my-svc-0-mi-0-leader-0-rct-0 → shared by leader-0-pod-0, leader-0-pod-1
  my-svc-0-mi-0-leader-1-rct-0 → shared by leader-1-pod-0, leader-1-pod-1
  my-svc-0-mi-1-leader-0-rct-0 → shared by leader-0-pod-0, leader-0-pod-1
  my-svc-0-mi-1-leader-1-rct-0 → shared by leader-1-pod-0, leader-1-pod-1
```

**For standalone PodCliques** (not in a PCSG), the PCLQ resource name is `<pcs>-<pcsIndex>-<pclqTemplate>`,
so the pattern is the same:

```
PCLQ PerInstance:
  my-svc-0-frontend-rct-0              → shared by all frontend pods

PCLQ PerReplica:
  my-svc-0-frontend-0-rct-1           → shared by frontend-0-pod-0, frontend-0-pod-1
  my-svc-0-frontend-1-rct-1           → shared by frontend-1-pod-0, frontend-1-pod-1
```

### Owner References

ResourceClaim ownership determines garbage collection behavior:

| Level + Scope | Owner | Rationale |
|---|---|---|
| PCSG `PerInstance` | PCSG object | Claim spans all replicas of the PCSG; GC'd when PCSG is deleted |
| PCSG `PerReplica` | PCSG object | Claim spans all PCLQs in a PCSG replica; GC'd when PCSG is deleted |
| PCLQ `PerInstance` | PCLQ object | Claim is per-PCLQ instance; GC'd when PCLQ is deleted |
| PCLQ `PerReplica` | PCLQ object | Created by the pod controller but logically belongs to the PCLQ; on scale-down, the controller must **explicitly delete** per-replica claims for removed replica slots (Kubernetes GC only fires on PCLQ deletion, not on replica reduction) |

### Common Types

```go
// ResourceAllocationScope defines the sharing scope for a ResourceAllocation.
// +kubebuilder:validation:Enum=PerInstance;PerReplica
type ResourceAllocationScope string

const (
	// ResourceAllocationScopePerInstance creates one ResourceClaim per instance of the
	// owning resource (PCLQ or PCSG), shared across all replicas and pods.
	ResourceAllocationScopePerInstance ResourceAllocationScope = "PerInstance"
	// ResourceAllocationScopePerReplica creates one ResourceClaim per replica, shared
	// across all pods within that replica.
	ResourceAllocationScopePerReplica ResourceAllocationScope = "PerReplica"
)

// ResourceAllocation defines a single shared ResourceClaim with a scope that determines
// how many ResourceClaim instances are created.
type ResourceAllocation struct {
	// Scope determines the sharing granularity. PerInstance creates one RC for the entire
	// resource instance. PerReplica creates one RC per replica.
	Scope ResourceAllocationScope `json:"scope"`
	// Spec is an inline ResourceClaimTemplate spec. Grove creates and manages ResourceClaim
	// objects directly from this spec.
	Spec resourcev1.ResourceClaimTemplateSpec `json:"spec"`
}

// PCSGResourceAllocation extends ResourceAllocation with a CliqueNames field that scopes
// which PodCliques in the scaling group receive the shared ResourceClaims.
type PCSGResourceAllocation struct {
	ResourceAllocation `json:",inline"`
	// CliqueNames limits which PodCliques in the scaling group receive the ResourceClaims.
	// If empty, all PodCliques in the group receive them.
	// +optional
	CliqueNames []string `json:"cliqueNames,omitempty"`
}
```

Each `ResourceAllocation` entry maps to exactly one ResourceClaim pattern: a single `Spec` and a `Scope`
that controls how many RC instances are created. Grove creates and fully manages `ResourceClaim` objects directly
from these specs — no intermediate `ResourceClaimTemplate` objects are created.
See [ResourceClaim Naming Convention](#resourceclaim-naming-convention) for the deterministic naming scheme and
[Owner References](#owner-references) for garbage collection semantics.

### PodClique-Level Resource Sharing

**API** 

```go
type PodCliqueTemplateSpec struct {
	// Name must be unique within a PodCliqueSet and is used to denote a role.
	// Once set it cannot be updated.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names
	Name string `json:"name"`
	...
	// ResourceAllocations defines shared ResourceClaims for this PodClique. Each entry
	// creates ResourceClaims at the granularity specified by its Scope:
	//   - PerInstance: one RC per PCLQ instance, shared by all replicas and pods
	//   - PerReplica: one RC per replica, shared by all pods within the replica
	// NOTE: This is not the same as adding ResourceClaimTemplate inside the
	// Spec.PodSpec.ResourceClaims[x].ResourceClaimTemplateName in the PodClique since that will
	// create a unique ResourceClaim for each pod in the PodClique.
	// +optional
	ResourceAllocations []ResourceAllocation `json:"resourceAllocations,omitempty"`
	// Specification of the desired behavior of a PodClique.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	Spec PodCliqueSpec `json:"spec"`
}
```

To enable resource sharing among `Pod`s within a `PodClique`, a new field `ResourceAllocations` will be added
to `PodCliqueTemplateSpec`. Each entry has a `Scope` (`PerInstance` or `PerReplica`) and a single inline
`ResourceClaimTemplateSpec`. All specs must be in the same namespace as the `PodCliqueSet`.

The PodClique reconciler will process `PerInstance` entries and create one `ResourceClaim` per entry.
`PerReplica` entries are passed to the pod controller, which creates one `ResourceClaim` per replica per entry.
All resource claims are then injected into the `PodSpec`.

**Example:**

The following example shows how to use `resourceAllocations` with `PerInstance` scope to share GPUs among all
pods within a single PodClique instance:

```yaml
apiVersion: grove.io/v1alpha1
kind: PodCliqueSet
metadata:
  name: shared-gpu-example
  namespace: default
spec:
  replicas: 2  # Creates 2 instances of the PodClique (each gets its own ResourceClaim)
  template:
    cliques:
      - name: inference
        resourceAllocations:
          - scope: PerInstance
            spec:
              spec:
                devices:
                  requests:
                    - name: gpu
                      deviceClassName: gpu.nvidia.com
                      count: 2
                  config:
                    - opaque:
                        driver: gpu.nvidia.com
                        parameters:
                          apiVersion: gpu.nvidia.com/v1alpha1
                          kind: GpuClaimParameters
                          sharing:
                            strategy: TimeSlicing
                            replicas: 4
        spec:
          roleName: inference
          replicas: 4  # All 4 pods share the same GPUs within each PCLQ instance
          podSpec:
            containers:
              - name: inference
                image: nvidia/cuda:12.0-runtime
                command: ["/bin/sh", "-c"]
                args:
                  - |
                    echo "Pod: $POD_NAME - Using shared GPU"
                    sleep infinity
                resources:
                  requests:
                    cpu: "1"
                    memory: "2Gi"
            restartPolicy: Always
```

In this example:
- The `PerInstance` scope creates one ResourceClaim per PodClique instance
- The inline spec defines 2 GPUs with time-slicing enabled
- All 4 pods within each PodClique instance share the same 2 GPUs
- The 2 PCS replicas maintain isolation (different ResourceClaims, different GPUs)



### PodCliqueScalingGroup-Level Resource Sharing

**API**

```go
// PodCliqueScalingGroupConfig is a group of PodClique's that are scaled together.
type PodCliqueScalingGroupConfig struct {
	// Name is the name of the PodCliqueScalingGroupConfig. This should be unique within the PodCliqueSet.
	// It allows consumers to give a semantic name to a group of PodCliques that needs to be scaled together.
	Name string `json:"name"`
	...
	// ResourceAllocations defines shared ResourceClaims at the PCSG level. Each entry
	// creates ResourceClaims at the granularity specified by its Scope:
	//   - PerInstance: one RC for the entire PCSG, shared across all replicas
	//   - PerReplica: one RC per PCSG replica, shared across all PCLQs in that replica
	// CliqueNames limits which PodCliques receive the claims (empty = all).
	// +optional
	ResourceAllocations []PCSGResourceAllocation `json:"resourceAllocations,omitempty"`
}
```

**Example:**

The following example demonstrates sharing resources across multiple PodCliques within a PodCliqueScalingGroup,
using `PerReplica` scope so each PCSG replica gets its own isolated ResourceClaim:

```yaml
apiVersion: grove.io/v1alpha1
kind: PodCliqueSet
metadata:
  name: training-pipeline
  namespace: default
spec:
  replicas: 1
  template:
    cliques:
      - name: data-preprocessor
        spec:
          roleName: preprocessor
          replicas: 2
          podSpec:
            containers:
              - name: preprocessor
                image: nvidia/cuda:12.0-runtime
                command: ["/bin/sh", "-c"]
                args:
                  - |
                    echo "Preprocessor pod: $POD_NAME"
                    echo "Loading data into GPU memory..."
                    sleep infinity
                resources:
                  requests:
                    cpu: "2"
                    memory: "4Gi"
            restartPolicy: Always
      - name: model-trainer
        spec:
          roleName: trainer
          replicas: 3
          podSpec:
            containers:
              - name: trainer
                image: nvidia/cuda:12.0-runtime
                command: ["/bin/sh", "-c"]
                args:
                  - |
                    echo "Training pod: $POD_NAME"
                    echo "Training model using preprocessed data from GPU memory..."
                    sleep infinity
                resources:
                  requests:
                    cpu: "4"
                    memory: "8Gi"
            restartPolicy: Always
    scalingGroups:
      - name: training-experiment
        replicas: 3
        cliqueNames:
          - data-preprocessor
          - model-trainer
        resourceAllocations:
          - scope: PerReplica
            spec:
              spec:
                devices:
                  requests:
                    - name: gpu
                      deviceClassName: gpu.nvidia.com
                      count: 4
                  config:
                    - opaque:
                        driver: gpu.nvidia.com
                        parameters:
                          apiVersion: gpu.nvidia.com/v1alpha1
                          kind: GpuClaimParameters
                          sharing:
                            strategy: MPS
                            maxClients: 8
            cliqueNames:
              - data-preprocessor
              - model-trainer
```

In this example:
- The `PerReplica` scope creates one ResourceClaim per PCSG replica
- The inline spec defines 4 GPUs with NVIDIA MPS for sharing
- 3 PCSG replicas create 3 independent training experiments
- Within each experiment (PCSG replica):
  - 2 preprocessing pods + 3 training pods = 5 total pods share the same 4 GPUs
  - All pods can access the same GPU memory space
- Each of the 3 experiments maintains isolation (different ResourceClaims, different GPU sets)



### Multiple Pods Per Replica

A `PodsPerReplica` field on `PodCliqueSpec` allows creating multiple pods per replica slot, orthogonal to resource
allocation. The primary use case is crash resilience (shadow pods): all pods in a replica slot are identical, and the
application uses lease-based election to determine which pod is active. When the active pod fails, another pod in the
same slot acquires the lease and takes over instantly.

**API**

```go
type PodCliqueSpec struct {
	...
	// PodsPerReplica is the number of pods to create per replica slot.
	// Default is 1 (current behavior). When > 1, pods use two-dimensional hostnames:
	// <pclq-name>-<replicaIndex>-pod-<podIndex>
	// All pods in a replica slot are identical — active-pod election is handled at the
	// application level via leases.
	// +optional
	// +kubebuilder:default=1
	PodsPerReplica *int32 `json:"podsPerReplica,omitempty"`
}
```

#### Pod Naming with Multiple Pods Per Replica

When `PodsPerReplica > 1`, pods use semantic hostnames encoding both the replica index and pod index:

- **PodsPerReplica = 1** (default): `<pclq-name>-<replicaIndex>` (unchanged)
- **PodsPerReplica > 1**: `<pclq-name>-<replicaIndex>-pod-<podIndex>`

All pods in a replica slot get the `-pod-` suffix. Pod slot index 0 is the first pod, pod index 1 is the
second, etc.

**Example** — `replicas: 2, podsPerReplica: 3` (3 pods per replica, 6 total):

```
pclq-0-pod-0  replica-index=0, pod-index=0
pclq-0-pod-1  replica-index=0, pod-index=1
pclq-0-pod-2  replica-index=0, pod-index=2
pclq-1-pod-0  replica-index=1, pod-index=0
pclq-1-pod-1  replica-index=1, pod-index=1
pclq-1-pod-2  replica-index=1, pod-index=2
```

#### Environment Variables

Grove injects the following env vars into all pods when `PodsPerReplica > 1`:

- `GROVE_REPLICA_INDEX` — the replica slot index
- `GROVE_POD_INDEX` — the index within the replica slot (0, 1, ..., PodsPerReplica-1)
- `GROVE_PODS_PER_REPLICA` — total pods per replica slot

These enable applications to construct peer hostnames via simple env var interpolation (no arithmetic):

```
leader_hostname = "<leader-pclq>-$(GROVE_REPLICA_INDEX)-pod-$(GROVE_POD_INDEX).$(GROVE_HEADLESS_SERVICE)"
```

`GROVE_PCLQ_POD_INDEX` (existing env var) equals `GROVE_REPLICA_INDEX` when `PodsPerReplica > 1`.

#### Multi-Node + Multiple Pods Per Replica

Within a PCSG replica, each pod maps 1:1 across cliques: pod K of every worker connects to pod K of
the leader. This allows all pods to pre-establish NCCL connections, making failover instant (no NCCL re-initialization).

**Note:** Dynamo currently hardcodes `-0` as the leader hostname. It will need updating to use the env var
interpolation above. This is a Dynamo-side change, not a Grove change.

### Per-Replica Resource Sharing

When `PodsPerReplica > 1`, all pods within a single replica slot may need to share a ResourceClaim that is isolated
from other replicas. This is achieved by using `PerReplica` scope in the PCLQ-level `ResourceAllocations`:

Per-replica ResourceClaims are owned by the PodClique instance. This ensures they survive individual pod re-creations
(important for crash recovery) but are garbage-collected when the PodClique is deleted. On scale-down, the controller
explicitly deletes per-replica claims for removed replica slots.

**Example:**

The following example shows a `PodCliqueSet` with multiple pods per replica and both `PerInstance` and `PerReplica`
resource sharing. The `PerInstance` claim is shared across all replicas, while the `PerReplica` claim is shared only
between all pods in the same replica slot.

```yaml
apiVersion: grove.io/v1alpha1
kind: PodCliqueSet
metadata:
  name: inference-with-shadows
  namespace: default
spec:
  replicas: 2
  template:
    cliques:
      - name: prefill-worker
        resourceAllocations:
          # PerInstance: one claim per PCLQ instance, shared across all replicas + pods
          - scope: PerInstance
            spec:
              spec:
                devices:
                  requests:
                    - name: gpu-pool
                      deviceClassName: gpu.nvidia.com
                      count: 8
          # PerReplica: one claim per replica slot, shared between all pods in the slot
          - scope: PerReplica
            spec:
              spec:
                devices:
                  requests:
                    - name: gpu-partition
                      deviceClassName: gpu.nvidia.com
                      count: 1
        spec:
          roleName: worker
          replicas: 3
          podsPerReplica: 2
          podSpec:
            containers:
              - name: worker
                image: nvidia/cuda:12.0-runtime
                command: ["/bin/sh", "-c"]
                args:
                  - |
                    echo "Worker pod: $POD_NAME"
                    sleep infinity
                resources:
                  requests:
                    cpu: "2"
                    memory: "4Gi"
            restartPolicy: Always
```

In this example:
- 2 PCS replicas create 2 PCLQ instances, each with its own `PerInstance` GPU pool ResourceClaim
- Within each PCLQ instance, 3 replicas x 2 pods = 6 pods share the `PerInstance` GPU pool
- Each replica slot's 2 pods additionally share a `PerReplica` GPU partition
- Pod hostnames: `prefill-worker-0-pod-0`, `prefill-worker-0-pod-1`, `prefill-worker-1-pod-0`, etc.
- If a pod in a replica slot fails, another pod in the same slot already has access to the same GPU partition

### Monitoring

<!--
This section contains details of events, metrics, status conditions and other status fields that will aid in determining health of the feature, or help measure any service level objectives that might be optionally defined.
-->

### Dependencies

Dynamic Resource Allocation (DRA) is a prerequisite for this GREP since it relies on the ResourceClaim API
to enable resource sharing. DRA graduated to *BETA* in *v1.32* and has been promoted to *GA* since Kubernetes *v1.34*. If you are using a Kubernetes version prior to v1.34, you will need to enable the `DynamicResourceAllocation` feature gate to use this feature. For Kubernetes
v1.34 and above, DRA is enabled by default, and you can use this feature without any additional configuration.

### Test Plan

<!--
For the functionality an epic (issue) should be created. Along with a sub-issue for the GREP, there should be a dedicated issue created for integration and e2e tests. This issue should have details of all scenarios that needs to be tested. Provide a link to issue(s) in this section.
-->

## Appendix

In case the readers are not familiar with DRA, the following links will help them get started:
* [Kubernetes DRA Official Documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
* [Dynamic Resource Allocation (DRA) KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4381-dra-structured-parameters)
