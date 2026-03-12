# Hierarchical Resource Sharing

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
    - [The Need for Multiple Sharing Scopes](#the-need-for-multiple-sharing-scopes)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
- [Proposal](#proposal)
    - [User Stories (*Optional*)](#user-stories-optional)
        - [Story 1: Resilient Inference with Shadow Pods and Per-Replica GPU Sharing](#story-1-resilient-inference-with-shadow-pods-and-per-replica-gpu-sharing)
        - [Story 2: Multi-Stage Training Pipeline with GPU Sharing](#story-2-multi-stage-training-pipeline-with-gpu-sharing)
    - [Limitations/Risks &amp; Mitigations](#limitationsrisks--mitigations)
- [Design Details](#design-details)
    - [Common Types](#common-types)
    - [PodClique-Level Resource Sharing](#podclique-level-resource-sharing)
    - [PodCliqueScalingGroup-Level Resource Sharing](#podcliquescalinggroup-level-resource-sharing)
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
group of pods at three levels of the Grove hierarchy by leveraging [ResourceClaim](https://github.com/kubernetes/api/blob/ffebe2b51dedadf6a36343b495ca26060cb7a93d/resource/v1/types.go#L741) and [ResourceClaimTemplate](https://github.com/kubernetes/api/blob/ffebe2b51dedadf6a36343b495ca26060cb7a93d/resource/v1/types.go#L1850) 
offered via Dynamic Resource Allocation (DRA) in Kubernetes. The design enables: 

* Pods within a single PCLQ instance to share resources (PCLQ-instance-level),
* A primary pod and its shadow pods within a single PCLQ replica to share resources (per-replica-level), or
* Pods across a subset of PCLQs within a PCSG instance to share resources (PCSG-level),

while ensuring proper isolation between replicas during scaling operations.

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
disaggregated inference workload with shadow pods for crash resilience:

- **PCSG-level**: An NVSwitch or interconnect resource shared across all PodCliques in a scaling group replica
  (e.g. a leader and its workers sharing a fabric).
- **PCLQ-instance-level**: A set of GPUs shared across all replicas of a PodClique instance (e.g. all worker replicas
  in a scaling group replica share one pool of GPUs).
- **Per-replica-level**: A GPU partition shared only between a primary pod and its shadow pods within a
  single replica, enabling zero-downtime recovery without resource reallocation.

These three scopes are orthogonal and composable. A single PodClique may participate in all three simultaneously.
Without per-replica sharing, shadow pods cannot share hardware with their primary pod without also sharing with every
other replica — defeating the isolation needed for independent recovery.

### Goals

- Enable users to define resource sharing primitives at multiple levels of Grove hierarchy, i.e. PodClique and
  PodCliqueScalingGroup.
- Users should be able to limit and scope resource sharing within subset of a group or within a specific level,
  e.g. share resource between pods of a PodClique instance vs between pods of a PCSG instance, or between a subset of
  PCLQs within a PCSG instance.
- Enable users to reference externally created ResourceClaimTemplates or provide inline ResourceClaimTemplateSpecs
  (or both) to be used for a resource sharing group.
- Enable per-replica resource sharing within a PodClique so that a primary pod and its shadow pods can share resources
  while maintaining isolation from other replicas.

### Non Goals

_(none at this time)_

## Proposal




### User Stories (*Optional*)

#### Story 1: Resilient Inference with Shadow Pods and Per-Replica GPU Sharing

A platform team deploys a disaggregated inference workload with a prefill leader (PCA, 3 replicas) and prefill workers
(PCB, 2 replicas). Each replica has 1 primary pod and 1 shadow pod for crash resilience. The shadow pod holds
references to the same GPU memory as the primary so that if the primary crashes, the shadow takes over instantly without
reloading model weights.

The workload requires three levels of resource sharing:

1. **PCSG-level** (`resourceClaimTemplateConfigs`): An NVSwitch fabric claim (RCT-M) shared across all pods in the
   scaling group replica (leader + workers).
2. **PCLQ-instance-level** (`resourceClaimTemplateConfig`): A GPU pool claim (RCT-N for PCA, RCT-P for PCB) shared
   across all replicas of a PodClique instance — e.g. all 3 PCA replicas and their shadows share one set of GPUs.
3. **Per-replica-level** (`shadow.resourceClaimTemplateConfig`): A GPU partition claim (RCT-SHD) shared only between
   the primary and shadow pods within a single replica — enabling each replica to recover independently.

_Challenge_: Without per-replica sharing, the shadow pod would either get its own exclusive GPU allocation (wasting
resources) or share with all replicas (breaking isolation). The per-replica scope fills this gap. Using a ResourceClaim
directly in the PodClique's pod template causes all pods across all PCSG replicas to reference the same claim, breaking
isolation. Using a ResourceClaimTemplate creates unique claims per pod, preventing any sharing at all.

_Solution_: Grove orchestrates resource sharing at each level of the hierarchy:

- `resourceClaimTemplateConfigs` at the PCSG level creates one ResourceClaim per PCSG replica, injected into all
  PodCliques in that replica.
- `resourceClaimTemplateConfig` at the PCLQ level creates one ResourceClaim per PCLQ instance, shared across all
  replicas and their shadows.
- `shadow.resourceClaimTemplateConfig` creates one ResourceClaim per PCLQ replica, shared only between the primary
  and shadow pods of that replica.

**Concrete example** of the ResourceClaim distribution:

```
PCS:
  cliques:
    - PCA: replicas=3, resourceClaimTemplateConfig={names: [RCT-N]},
           shadow={count: 1, resourceClaimTemplateConfig: {names: [RCT-SHD]}}
    - PCB: replicas=2, resourceClaimTemplateConfig={names: [RCT-P]},
           shadow={count: 1, resourceClaimTemplateConfig: {names: [RCT-SHD]}}
  scalingGroups:
    - SGX: {PCA, PCB}, resourceClaimTemplateConfigs=[{names: [RCT-M], cliqueNames: [PCA, PCB]}]

SGX-0: RC-M0   (PCSG-level — shared by ALL pods in SGX-0)
  SGX-0-PCA: RC-N0   (PCLQ-instance-level — shared by all 6 pods in PCA)
    {SGX-0-PCA-0-0, SGX-0-PCA-0-1} → RC-SHD-SGX-0-PCA-0   (per-replica)
    {SGX-0-PCA-1-0, SGX-0-PCA-1-1} → RC-SHD-SGX-0-PCA-1   (per-replica)
    {SGX-0-PCA-2-0, SGX-0-PCA-2-1} → RC-SHD-SGX-0-PCA-2   (per-replica)
  SGX-0-PCB: RC-P0   (PCLQ-instance-level — shared by all 4 pods in PCB)
    {SGX-0-PCB-0-0, SGX-0-PCB-0-1} → RC-SHD-SGX-0-PCB-0   (per-replica)
    {SGX-0-PCB-1-0, SGX-0-PCB-1-1} → RC-SHD-SGX-0-PCB-1   (per-replica)

SGX-1: RC-M1
  SGX-1-PCA: RC-N1
    {SGX-1-PCA-0-0, SGX-1-PCA-0-1} → RC-SHD-SGX-1-PCA-0   (per-replica)
    {SGX-1-PCA-1-0, SGX-1-PCA-1-1} → RC-SHD-SGX-1-PCA-1   (per-replica)
    {SGX-1-PCA-2-0, SGX-1-PCA-2-1} → RC-SHD-SGX-1-PCA-2   (per-replica)
  SGX-1-PCB: RC-P1
    {SGX-1-PCB-0-0, SGX-1-PCB-0-1} → RC-SHD-SGX-1-PCB-0   (per-replica)
    {SGX-1-PCB-1-0, SGX-1-PCB-1-1} → RC-SHD-SGX-1-PCB-1   (per-replica)
```

In this example:
- Pod naming: `{PCSG-replica}-{PCLQ-name}-{replica-index}-{shadow-index}` where shadow-index 0 is the primary
- RC-M0/RC-M1 are PCSG-level claims: one per PCSG replica, shared by every pod in that replica
- RC-N0/RC-P0 are PCLQ-instance-level claims: one per PCLQ instance, shared by all replicas and shadows of that PCLQ
- RC-SHD-* are per-replica claims: one per replica, shared only between the primary and shadow pods of that replica

#### Story 2: Multi-Stage Training Pipeline with GPU Sharing

Multi-stage ML pipelines with separate preprocessing and training components are a common pattern in production ML systems. Frameworks like [Kubeflow Pipelines](https://www.kubeflow.org/docs/components/pipelines/v1/introduction/), [TensorFlow Extended (TFX)](https://www.tensorflow.org/tfx), and [Ray Train](https://docs.ray.io/en/latest/train/train.html) enable users to define pipelines where data preprocessing (ETL, feature engineering, augmentation) runs as separate containers/pods from the training workload.

In such a distributed training pipeline, data preprocessing pods load and transform data into GPU memory, while model training pods consume this preprocessed data directly from GPU memory without expensive CPU-GPU transfers. Libraries like [NVIDIA DALI](https://docs.nvidia.com/deeplearning/dali/user-guide/docs/index.html) provide GPU-accelerated data preprocessing capabilities that make this pattern efficient. The preprocessing and training pods are modeled as separate PCLQs within a PCSG, where each PCSG replica represents a different training experiment.

_Challenge_: Each experiment (PCSG instance) needs its own isolated set of GPUs, but within an experiment, both preprocessing and training pods should share the same GPU devices for efficient data transfer and memory utilization. Standard GPU allocation creates exclusive claims per pod, preventing this sharing pattern. When these stages need to share GPUs for zero-copy data transfer and to avoid CPU-GPU memory copying overhead, DRA's [shareable ResourceClaims](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#shareable-resources) become essential.

_Solution_: By leveraging GPU sharing technologies like [NVIDIA Multi-Process Service (MPS)](https://docs.nvidia.com/deploy/mps/index.html) for efficient GPU sharing or [CUDA IPC (Inter-Process Communication)](https://docs.nvidia.com/cuda/cuda-c-programming-guide/index.html#interprocess-communication) for sharing GPU memory between processes, along with techniques like [GPU Direct Storage](https://developer.nvidia.com/gpudirect-storage) for direct data paths, Grove enables this pattern through `ResourceClaimTemplateConfigs` at the PCSG level. By specifying `resourceClaimTemplateConfigs` with `cliqueNames` referencing both the preprocessing and training PCLQs, Grove creates a ResourceClaim per PCSG instance that is shared across the specified PCLQs. This enables both pod types to access the same GPU devices within each experiment while maintaining isolation across different experiments.

### Limitations/Risks & Mitigations

<!-- 
What are the current set of limitations or risks of this proposal? Think broadly by considering the impact of the changes proposed on kubernetes ecosystem. Optionally mention ways to mitigate these.
-->

## Design Details

### Common Types

```go
// ResourceClaimTemplateConfig defines the sources for creating shared ResourceClaims.
// Users may reference externally-created ResourceClaimTemplates by name, provide inline
// ResourceClaimTemplateSpecs, or both. At least one of Names or Specs must be non-empty.
type ResourceClaimTemplateConfig struct {
	// Names is a list of externally-created ResourceClaimTemplate names.
	// +optional
	Names []string `json:"names,omitempty"`
	// Specs is a list of inline ResourceClaimTemplate specs. Grove creates and manages
	// ResourceClaims directly from these specs, removing the need for users to create
	// ResourceClaimTemplate objects separately.
	// +optional
	Specs []resourcev1.ResourceClaimTemplateSpec `json:"specs,omitempty"`
}
```

`ResourceClaimTemplateConfig` supports two workflows for defining shared resources:

- **External templates**: Users create `ResourceClaimTemplate` objects themselves (e.g. managed by Helm, GitOps, or a
  platform team) and reference them by name via `names`. Grove creates `ResourceClaim` objects from these templates.
- **Inline specs**: Users provide `ResourceClaimTemplateSpec` definitions directly via `specs`. Grove creates and fully
  manages both the `ResourceClaimTemplate` and resulting `ResourceClaim` objects, removing the need for users to create
  templates separately.

Both fields are optional, but at least one must be non-empty. When both are set, ResourceClaims are created from all
sources — users can reference a shared platform-managed GPU template by name while also defining a workload-specific
inline spec in the same config.

### PodClique-Level Resource Sharing

**API** 

```go
type PodCliqueTemplateSpec struct {
	// Name must be unique within a PodCliqueSet and is used to denote a role.
	// Once set it cannot be updated.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names
	Name string `json:"name"`
	...
	// ResourceClaimTemplateConfig defines ResourceClaimTemplate sources for creating ResourceClaims
	// shared across all pods in the PodClique instance. Users can reference externally-created
	// ResourceClaimTemplates by name, provide inline specs, or both.
	// NOTE: This is not the same as adding ResourceClaimTemplate inside the
	// Spec.PodSpec.ResourceClaims[x].ResourceClaimTemplateName in the PodClique since that will
	// create a unique ResourceClaim for each pod in the PodClique.
	// +optional
	ResourceClaimTemplateConfig *ResourceClaimTemplateConfig `json:"resourceClaimTemplateConfig,omitempty"`
	// Specification of the desired behavior of a PodClique.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	Spec PodCliqueSpec `json:"spec"`
}
```

To enable resource sharing among `Pod`s within a `PodClique`, a new field `ResourceClaimTemplateConfig` will be added
to `PodCliqueTemplateSpec`. Users can reference externally-created `ResourceClaimTemplate` objects by name, provide
inline `ResourceClaimTemplateSpec` definitions, or both. All `ResourceClaimTemplate`s (external or inline) must be in
the same namespace as the `PodCliqueSet`.

The PodClique reconciler will process the `ResourceClaimTemplateConfig` and for each template (by name or inline spec)
it will create a `ResourceClaim`. All of the resource claims will then be configured in the `PodSpec`.

**Example:**

The following example shows how to use `resourceClaimTemplateConfig` to enable resource sharing among pods within a
single PodClique instance.

First, create a `ResourceClaimTemplate` that defines the GPU resources to be shared:

```yaml
apiVersion: resource.k8s.io/v1alpha3
kind: ResourceClaimTemplate
metadata:
  name: gpu-claim-template
  namespace: default
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
```

Then, reference this template in your `PodCliqueSet` to enable sharing within a PodClique:

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
        resourceClaimTemplateConfig:
          names:
            - gpu-claim-template
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
- The `gpu-claim-template` defines 2 GPUs with time-slicing enabled
- Each PodClique instance gets its own `ResourceClaim` created from the template
- All 4 pods within each PodClique instance share the same 2 GPUs
- The 2 PCS replicas maintain isolation (different ResourceClaims, different GPUs)



### PodCliqueScalingGroup-Level Resource Sharing

**API**

```go
// ScopedResourceClaimTemplateConfig extends ResourceClaimTemplateConfig with a CliqueNames
// field that scopes which PodCliques in the scaling group receive the shared ResourceClaims.
type ScopedResourceClaimTemplateConfig struct {
	ResourceClaimTemplateConfig `json:",inline"`
	// CliqueNames limits which PodCliques in the scaling group receive the ResourceClaims.
	// If empty, all PodCliques in the group receive them.
	// +optional
	CliqueNames []string `json:"cliqueNames,omitempty"`
}
```

```go
// PodCliqueScalingGroupConfig is a group of PodClique's that are scaled together.
// Each member PodClique.Replicas will be computed as a product of PodCliqueScalingGroupConfig.Replicas and PodCliqueTemplateSpec.Spec.Replicas.
// NOTE: If a PodCliqueScalingGroupConfig is defined, then for the member PodClique's, individual AutoScalingConfig cannot be defined.
type PodCliqueScalingGroupConfig struct {
	// Name is the name of the PodCliqueScalingGroupConfig. This should be unique within the PodCliqueSet.
	// It allows consumers to give a semantic name to a group of PodCliques that needs to be scaled together.
	Name string `json:"name"`
	...
	// ResourceClaimTemplateConfigs is a list of ScopedResourceClaimTemplateConfig which defines
	// ResourceClaimTemplate sources for a set of PodCliques in the scaling group. A ResourceClaim
	// is created per template and added to the PodSpec of each PodClique specified in the CliqueNames
	// field. This allows sharing of resources such as accelerators across all pods in the specified
	// PodCliques that are part of one PodCliqueScalingGroup instance.
	ResourceClaimTemplateConfigs []ScopedResourceClaimTemplateConfig `json:"resourceClaimTemplateConfigs,omitempty"`
}
```

**Example:**

The following example demonstrates sharing resources across multiple PodCliques within a PodCliqueScalingGroup.

First, create a `ResourceClaimTemplate` for shared GPUs:

```yaml
apiVersion: resource.k8s.io/v1alpha3
kind: ResourceClaimTemplate
metadata:
  name: shared-gpu-template
  namespace: default
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
                strategy: MPS  # NVIDIA Multi-Process Service
                maxClients: 8
```

Then, reference it in a `PodCliqueSet` with a scaling group that shares GPUs across preprocessing and training pods:

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
      # Preprocessing PodClique
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
      
      # Training PodClique
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
    
    # Define scaling group with shared resources
    scalingGroups:
      - name: training-experiment
        replicas: 3  # Creates 3 training experiments
        cliqueNames:
          - data-preprocessor
          - model-trainer
        # Share GPUs across both preprocessing and training pods within each experiment
        resourceClaimTemplateConfigs:
          - names:
              - shared-gpu-template
            cliqueNames:
              - data-preprocessor
              - model-trainer
```

In this example:
- The `shared-gpu-template` defines 4 GPUs with NVIDIA MPS for sharing
- 3 PCSG replicas create 3 independent training experiments
- Within each experiment (PCSG instance):
  - 2 preprocessing pods + 3 training pods = 5 total pods share the same 4 GPUs
  - All pods can access the same GPU memory space
- Each of the 3 experiments maintains isolation (different ResourceClaims, different GPU sets)



### Per-Replica Resource Sharing

When shadow pods are configured for a PodClique, all pods within a single replica (1 primary + N shadows) may need to
share a ResourceClaim that is isolated from other replicas. This introduces a third sharing scope that sits between the
PCLQ-instance level (shared across all replicas) and the per-pod level (no sharing at all).

In resilient inference deployments, each replica's shadow pod must hold a reference to the same GPU memory as the
primary pod so it can take over instantly upon failure. The shadow pod cannot share a GPU claim with other replicas
(that would break isolation and defeat the purpose of independent recovery), nor can it have its own exclusive claim
(that would waste resources). A per-replica ResourceClaim — shared between the primary and shadow pods of exactly one
replica — is the right granularity.

**API**

```go
// ShadowConfig configures shadow pods for a PodClique. Shadow pods share the same replica
// slot as the primary pod and can take over instantly upon failure.
type ShadowConfig struct {
	// Count is the number of shadow pods per replica.
	Count int `json:"count"`
	// ResourceClaimTemplateConfig defines ResourceClaimTemplate sources for creating per-replica
	// ResourceClaims shared between the primary and its shadow pods. Users can reference
	// externally-created ResourceClaimTemplates by name, provide inline specs, or both.
	// +optional
	ResourceClaimTemplateConfig *ResourceClaimTemplateConfig `json:"resourceClaimTemplateConfig,omitempty"`
}
```

```go
type PodCliqueSpec struct {
	...
	// Shadow configures shadow pods for crash resilience. When set, each replica gets Count
	// additional shadow pods that share per-replica ResourceClaims with the primary pod.
	// +optional
	Shadow *ShadowConfig `json:"shadow,omitempty"`
}
```

Per-replica ResourceClaims are owned by the PodClique instance. This ensures they survive individual pod re-creations
(important for shadow recovery) but are garbage-collected when the PodClique is deleted.

**Example:**

The following example shows a `PodCliqueSet` with shadow pods and per-replica resource sharing. The PCLQ-instance-level
claim (`gpu-pool-template`) is shared across all replicas, while the per-replica claim is defined inline and shared
only between the primary and its shadow pod.

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
        # PCLQ-instance-level: one claim per PCLQ instance, shared across all replicas + shadows
        resourceClaimTemplateConfig:
          names:
            - gpu-pool-template
        spec:
          roleName: worker
          replicas: 3
          shadow:
            count: 1
            # Per-replica: one claim per replica, shared between primary + shadow only
            resourceClaimTemplateConfig:
              specs:
                - spec:
                    devices:
                      requests:
                        - name: gpu-partition
                          deviceClassName: gpu.nvidia.com
                          count: 1
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
- 2 PCS replicas create 2 PCLQ instances, each with its own `gpu-pool-template` ResourceClaim
- Within each PCLQ instance, 3 replicas x 2 pods (1 primary + 1 shadow) = 6 pods share the instance-level GPU pool
- Each replica's primary + shadow pair additionally shares a per-replica GPU partition defined inline
- If a primary pod crashes, the shadow pod already has access to the same GPU partition and can take over immediately

### Monitoring

<!--
This section contains details of events, metrics, status conditions and other status fields that will aid in determining health of the feature, or help measure any service level objectives that might be optionally defined.
-->

### Dependencies

Dynamic Resource Allocation (DRA) is a prerequisite for this GREP since it relies on ResourceClaim and
ResourceClaimTemplate APIs to enable resource sharing. DRA graduated to *BETA* in *v1.32* and has been promoted to *GA* since Kubernetes *v1.34*. If you are using a Kubernetes version prior to v1.34, you will need to enable the `DynamicResourceAllocation` feature gate to use this feature. For Kubernetes
v1.34 and above, DRA is enabled by default, and you can use this feature without any additional configuration.

### Test Plan

<!--
For the functionality an epic (issue) should be created. Along with a sub-issue for the GREP, there should be a dedicated issue created for integration and e2e tests. This issue should have details of all scenarios that needs to be tested. Provide a link to issue(s) in this section.
-->

## Appendix

In case the readers are not familiar with DRA, the following links will help them get started:
* [Kubernetes DRA Official Documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
* [Dynamic Resource Allocation (DRA) KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4381-dra-structured-parameters)
