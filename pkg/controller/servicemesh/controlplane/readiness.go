package controlplane

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	"github.com/maistra/istio-operator/pkg/controller/common"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const statusAnnotationReadyComponentCount = "readyComponentCount"

func (r *controlPlaneInstanceReconciler) UpdateReadiness(ctx context.Context) error {
	log := common.LogFromContext(ctx)
	update, err := r.updateReadinessStatus(ctx)
	if update && !r.skipStatusUpdate() {
		statusErr := r.PostStatus(ctx)
		if statusErr != nil {
			// original error is more important than the status update error
			if err == nil {
				// if there's no original error, we can return the status update error
				return statusErr
			}
			// otherwise, we must log the status update error and return the original error
			log.Error(statusErr, "Error updating status")
		}
	}
	return err
}

func (r *controlPlaneInstanceReconciler) updateReadinessStatus(ctx context.Context) (bool, error) {
	log := common.LogFromContext(ctx)
	log.Info("Updating ServiceMeshControlPlane readiness state")
	readinessMap, err := r.calculateComponentReadiness(ctx)
	if err != nil {
		condition := v1.Condition{
			Type:    v1.ConditionTypeReady,
			Status:  v1.ConditionStatusUnknown,
			Reason:  v1.ConditionReasonProbeError,
			Message: fmt.Sprintf("Error collecting ready state: %s", err),
		}
		r.Status.SetCondition(condition)
		r.EventRecorder.Event(r.Instance, corev1.EventTypeWarning, eventReasonNotReady, condition.Message)
		return true, err
	}

	readyComponents := sets.NewString()
	unreadyComponents := sets.NewString()
	for component, ready := range readinessMap {
		if ready {
			readyComponents.Insert(component)
		} else {
			log.Info(fmt.Sprintf("%s resources are not fully available", component))
			unreadyComponents.Insert(component)
		}
	}
	readyCondition := r.Status.GetCondition(v1.ConditionTypeReady)
	updateStatus := false
	if len(unreadyComponents) > 0 {
		if readyCondition.Status != v1.ConditionStatusFalse {
			condition := v1.Condition{
				Type:    v1.ConditionTypeReady,
				Status:  v1.ConditionStatusFalse,
				Reason:  v1.ConditionReasonComponentsNotReady,
				Message: "Some components are not fully available",
			}
			r.Status.SetCondition(condition)
			r.EventRecorder.Event(r.Instance, corev1.EventTypeWarning, eventReasonNotReady, fmt.Sprintf("The following components are not fully available: %s", unreadyComponents))
			updateStatus = true
		}
	} else {
		if readyCondition.Status != v1.ConditionStatusTrue {
			condition := v1.Condition{
				Type:    v1.ConditionTypeReady,
				Status:  v1.ConditionStatusTrue,
				Reason:  v1.ConditionReasonComponentsReady,
				Message: "All component deployments are Available",
			}
			r.Status.SetCondition(condition)
			r.EventRecorder.Event(r.Instance, corev1.EventTypeNormal, eventReasonReady, condition.Message)
			updateStatus = true
		}
	}

	if r.Status.Annotations == nil {
		r.Status.Annotations = map[string]string{}
	}
	r.Status.Annotations[statusAnnotationReadyComponentCount] = fmt.Sprintf("%d/%d", len(readyComponents), len(readinessMap))

	return updateStatus, nil
}

type isReadyFunc func(runtime.Object) bool

func (r *controlPlaneInstanceReconciler) calculateComponentReadiness(ctx context.Context) (map[string]bool, error) {
	readinessMap := map[string]bool{}
	typesToCheck := map[runtime.Object]isReadyFunc{
		&appsv1.DeploymentList{}: func(obj runtime.Object) bool {
			deployment := obj.(*appsv1.Deployment)
			for _, condition := range deployment.Status.Conditions {
				if condition.Type == appsv1.DeploymentAvailable {
					return condition.Status == corev1.ConditionTrue
				}
			}
			return false
		},
		&appsv1.StatefulSetList{}: func(obj runtime.Object) bool {
			statefulSet := obj.(*appsv1.StatefulSet)
			return statefulSet.Status.ReadyReplicas >= statefulSet.Status.Replicas
		},
		&appsv1.DaemonSetList{}: func(obj runtime.Object) bool {
			daemonSet := obj.(*appsv1.DaemonSet)
			return r.daemonSetReady(daemonSet)
		},
	}
	for list, readyFunc := range typesToCheck {
		err := r.calculateReadinessForType(ctx, list, readyFunc, readinessMap)
		if err != nil {
			return readinessMap, err
		}
	}

	cniReady, err := r.isCNIReady(ctx)
	readinessMap["cni"] = cniReady
	return readinessMap, err
}

func (r *controlPlaneInstanceReconciler) isCNIReady(ctx context.Context) (bool, error) {
	if !r.cniConfig.Enabled {
		return true, nil
	}
	labelSelector := map[string]string{"istio": "cni"}
	daemonSets := &appsv1.DaemonSetList{}
	operatorNamespace := common.GetOperatorNamespace()
	if err := r.Client.List(ctx, client.MatchingLabels(labelSelector).InNamespace(operatorNamespace), daemonSets); err != nil {
		return false, err
	}
	for _, ds := range daemonSets.Items {
		if !r.daemonSetReady(&ds) {
			return false, nil
		}
	}
	return true, nil
}

func (r *controlPlaneInstanceReconciler) calculateReadinessForType(ctx context.Context, list runtime.Object, isReady isReadyFunc, readinessMap map[string]bool) error {
	log := common.LogFromContext(ctx)

	meshNamespace := r.Instance.GetNamespace()
	selector := map[string]string{common.OwnerKey: meshNamespace}
	err := r.Client.List(ctx, client.InNamespace(meshNamespace).MatchingLabels(selector), list)
	if err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}

	for _, obj := range items {
		metaObject, err := meta.Accessor(obj)
		if err != nil {
			return err
		}
		if component, ok := metaObject.GetLabels()[common.KubernetesAppComponentKey]; ok {
			ready, exists := readinessMap[component]
			readinessMap[component] = (ready || !exists) && isReady(obj)
		} else {
			// how do we have an owned resource with no component label?
			log.Error(nil, "skipping resource for readiness check: resource has no component label", obj.GetObjectKind(), metaObject.GetName())
		}
	}
	return nil
}

func (r *controlPlaneInstanceReconciler) daemonSetReady(ds *appsv1.DaemonSet) bool {
	return ds.Status.NumberUnavailable == 0
}
