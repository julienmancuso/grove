// /*
// Copyright 2025 The Grove Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

package podclique

import (
	"context"
	"fmt"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	k8sutils "github.com/NVIDIA/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) reconcileStatus(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	pgsName := componentutils.GetPodGangSetName(pclq.ObjectMeta)

	// mutate PodClique Status Replicas, ReadyReplicas, ScheduleGatedReplicas and UpdatedReplicas.
	if err := r.mutateStatusReplicaCounts(ctx, logger, pgsName, pclq); err != nil {
		logger.Error(err, "failed to mutate PodClique status with replica counts")
		return ctrlcommon.ReconcileWithErrors("failed to mutate PodClique status with replica counts", err)
	}

	// mutate the grovecorev1alpha1.ConditionTypeMinAvailableBreached condition based on the number of ready pods.
	// Only do this if the PodClique has been successfully reconciled at least once. This prevents prematurely setting
	// incorrect MinAvailable breached condition.
	if pclq.Status.ObservedGeneration != nil {
		mutateMinAvailableBreachedCondition(pclq)
	}

	// mutate the selector that will be used by an autoscaler.
	if err := mutateSelector(pgsName, pclq); err != nil {
		logger.Error(err, "failed to update selector for PodClique")
		return ctrlcommon.ReconcileWithErrors("failed to set selector for PodClique", err)
	}

	// update the PodClique status.
	if err := r.client.Status().Update(ctx, pclq); err != nil {
		logger.Error(err, "failed to update PodClique status")
		return ctrlcommon.ReconcileWithErrors("failed to update PodClique status", err)
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) mutateStatusReplicaCounts(ctx context.Context, logger logr.Logger, pgsName string, pclq *grovecorev1alpha1.PodClique) error {
	pods, err := componentutils.GetPCLQPods(ctx, r.client, pgsName, pclq)
	if err != nil {
		logger.Error(err, "failed to list pods for PodClique")
		return err
	}

	nonTerminatingPods := lo.Filter(pods, func(pod *corev1.Pod, _ int) bool {
		return !k8sutils.IsResourceTerminating(pod.ObjectMeta)
	})

	// mutate the PCLQ status with current number of schedule gated, ready pods and updated pods.
	pclq.Status.Replicas = int32(len(nonTerminatingPods))
	readyPods, scheduleGatedPods := getReadyAndScheduleGatedPods(nonTerminatingPods)
	pclq.Status.ReadyReplicas = int32(len(readyPods))
	pclq.Status.ScheduleGatedReplicas = int32(len(scheduleGatedPods))
	// TODO: change this when rolling update is implemented
	pclq.Status.UpdatedReplicas = int32(len(nonTerminatingPods))

	return nil
}

func mutateSelector(pgsName string, pclq *grovecorev1alpha1.PodClique) error {
	if pclq.Spec.ScaleConfig == nil {
		return nil
	}
	labels := lo.Assign(
		k8sutils.GetDefaultLabelsForPodGangSetManagedResources(pgsName),
		map[string]string{
			grovecorev1alpha1.LabelPodClique: pclq.Name,
		},
	)
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: labels})
	if err != nil {
		return fmt.Errorf("%w: failed to create label selector for PodClique %v", err, client.ObjectKeyFromObject(pclq))
	}
	pclq.Status.Selector = ptr.To(selector.String())
	return nil
}

func mutateMinAvailableBreachedCondition(pclq *grovecorev1alpha1.PodClique) {
	newCondition := computeMinAvailableBreachedCondition(pclq)
	if k8sutils.HasConditionChanged(pclq.Status.Conditions, newCondition) {
		meta.SetStatusCondition(&pclq.Status.Conditions, newCondition)
	}
}

func getReadyAndScheduleGatedPods(pods []*corev1.Pod) (readyPods []*corev1.Pod, scheduleGatedPods []*corev1.Pod) {
	for _, pod := range pods {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyPods = append(readyPods, pod)
			} else if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonSchedulingGated {
				scheduleGatedPods = append(scheduleGatedPods, pod)
			}
		}
	}
	return
}

func computeMinAvailableBreachedCondition(pclq *grovecorev1alpha1.PodClique) metav1.Condition {
	readyPods := pclq.Status.ReadyReplicas
	minAvailable := pclq.Spec.MinAvailable
	now := metav1.Now()

	if minAvailable == nil {
		return metav1.Condition{
			Type:               grovecorev1alpha1.ConditionTypeMinAvailableBreached,
			Status:             metav1.ConditionUnknown,
			Reason:             "MinAvailableNil",
			Message:            "MinAvailable is nil, cannot determine if the condition is breached",
			LastTransitionTime: now,
		}
	}
	if readyPods < *minAvailable {
		return metav1.Condition{
			Type:               grovecorev1alpha1.ConditionTypeMinAvailableBreached,
			Status:             metav1.ConditionTrue,
			Reason:             "InsufficientReadyPods",
			Message:            fmt.Sprintf("Insufficient ready pods. expected at least: %d, found: %d", *minAvailable, readyPods),
			LastTransitionTime: now,
		}
	}
	return metav1.Condition{
		Type:               grovecorev1alpha1.ConditionTypeMinAvailableBreached,
		Status:             metav1.ConditionFalse,
		Reason:             "SufficientReadyPods",
		Message:            fmt.Sprintf("Sufficient ready pods found. expected at least: %d, found: %d", *minAvailable, readyPods),
		LastTransitionTime: now,
	}
}
