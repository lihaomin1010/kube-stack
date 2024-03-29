/*
Copyright 2022.

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

package podmarker

import (
	"bytes"
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/jsonpath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	podmarkerv1 "kube-stack.me/apis/podmarker/v1"
)

// PodMarkerReconciler reconciles a PodMarker object
type PodMarkerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

var (
	llog = ctrl.Log.WithName("podMarkerReconciler")
)

//+kubebuilder:rbac:groups=podmarker.kube-stack.me,resources=podmarkers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=podmarker.kube-stack.me,resources=podmarkers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=podmarker.kube-stack.me,resources=podmarkers/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PodMarker object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *PodMarkerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var podMarkers podmarkerv1.PodMarkerList
	if err := r.List(ctx, &podMarkers, client.InNamespace(req.Namespace)); err != nil {
		llog.Error(err, "unable to list podMarkers")
		return ctrl.Result{}, err
	}

	if len(podMarkers.Items) > 0 {
		llog.Info("Reconcile", "namespace", req.Namespace, "number of podmarkers", len(podMarkers.Items))
	}

	hasError := false
	for _, pm := range podMarkers.Items {
		if err := r.processPodMarker(ctx, &pm, req.Namespace); err != nil {
			hasError = true
			llog.Error(err, "error when processing podmarker", "podmarker", pm)
		}
	}

	if hasError {
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}
	return ctrl.Result{}, nil
}

func (r *PodMarkerReconciler) processPodMarker(ctx context.Context, pm *podmarkerv1.PodMarker, namespace string) error {
	podList := make([]*corev1.Pod, 0)
	{
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(pm.Spec.Selector.MatchLabels)); err != nil {
			llog.Error(err, "unable to list pods")
			return err
		}
		for i := range pods.Items {
			podList = append(podList, &pods.Items[i])
		}
		sort.SliceStable(podList, func(i, j int) bool {
			return podList[i].CreationTimestamp.Time.Before(podList[i].CreationTimestamp.Time)
		})
	}
	llog.Info("processPodMarker", "podmarker name", pm.Name, "number of pods", len(podList))

	for i := range podList {
		changed := false
		for key, val := range pm.Spec.AddLabels {
			v := extractValueByJsonPath(podList[i], val)
			if podList[i].Labels[key] != v {
				podList[i].Labels[key] = v
				changed = true
			}
		}
		if changed {
			if err := r.Update(ctx, podList[i]); err != nil {
				llog.Error(err, "update pod")
				return err
			}
		}
	}

	podIndex := 0
	for _, value := range pm.Spec.MarkLabel.Values {
		count := value.Replicas
		if count <= 0 {
			count = len(podList) * value.Weight / 100.0
		}
		for i := 0; i < count && podIndex < len(podList); i++ {
			if podList[podIndex].Labels[pm.Spec.MarkLabel.Name] != value.Value {
				podList[podIndex].Labels[pm.Spec.MarkLabel.Name] = value.Value
				if err := r.Update(ctx, podList[podIndex]); err != nil {
					llog.Error(err, "update pod")
					return err
				}
			}
			podIndex++
		}
	}

	for podIndex < len(podList) {
		if podList[podIndex].Labels[pm.Spec.MarkLabel.Name] != "" {
			podList[podIndex].Labels[pm.Spec.MarkLabel.Name] = ""
			if err := r.Update(ctx, podList[podIndex]); err != nil {
				llog.Error(err, "update pod")
				return err
			}
		}
		podIndex++
	}

	return nil
}

func extractValueByJsonPath(pod *corev1.Pod, jsonPathExpr string) string {
	var (
		err    error
		unstct map[string]interface{}
	)

	if unstct, err = runtime.DefaultUnstructuredConverter.ToUnstructured(pod); err != nil {
		llog.Error(err, "runtime.DefaultUnstructuredConverter.ToUnstructured(pod)")
		return ""
	}

	j := jsonpath.New("")
	j.AllowMissingKeys(true)
	if err = j.Parse(jsonPathExpr); err != nil {
		llog.Error(err, "jsonpath parse err")
		return ""
	}
	buf := new(bytes.Buffer)
	if err = j.Execute(buf, unstct); err != nil {
		llog.Error(err, "jsonpath exec err")
		return ""
	}

	if len(validation.IsValidLabelValue(buf.String())) <= 0 {
		return buf.String()
	}
	return strings.Replace(base64.StdEncoding.EncodeToString(buf.Bytes()), "=", "", -1)
}

func (r *PodMarkerReconciler) findObjectForPodMaker(pod client.Object) []reconcile.Request {
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: pod.GetNamespace(),
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodMarkerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&podmarkerv1.PodMarker{}).
		Watches(
			&source.Kind{Type: &corev1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(r.findObjectForPodMaker),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).Complete(r)
}
