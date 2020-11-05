/*


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

package controllers

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	podmigv1 "github.com/schrej/podmigration-operator/api/v1"
)

const (
	podOwnerKey           = ".metadata.controller"
	migratingPodFinalizer = "podmig.schrej.net/Migrate"
)

var apiGVStr = podmigv1.GroupVersion.String()

// MigratingPodReconciler reconciles a MigratingPod object
type MigratingPodReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=podmig.schrej.net,resources=migratingpods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=podmig.schrej.net,resources=migratingpods/status,verbs=get;update;patch

func (r *MigratingPodReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("migratingpod", req.NamespacedName)

	var migratingPod podmigv1.MigratingPod
	if err := r.Client.Get(ctx, req.NamespacedName, &migratingPod); err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			log.Error(err, "unable to fetch MigratingPod")
			return ctrl.Result{}, err
		}
		log.Info("cleaning up orphan pods")
		if err := r.cleanUpOrphanPods(ctx, req.Namespace, req.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	childPods, err := r.getChildPods(ctx, req.Namespace, req.Name)
	if err != nil {
		log.Error(err, "unable to list child Pods")
		return ctrl.Result{}, err
	}

	// Now,
	// how to figure out phase?
	// when to create Pod?
	// when to migrate?
	// how to figure out "current" Pod?
	// how to track which one is source, which is target?
	// what to track in Status?

	log.Info("", "child pods count", len(childPods.Items))

	switch len(childPods.Items) {
	case 0:
		// create pod
		pod := GetPodFromTemplate(&migratingPod.Spec.Template, &migratingPod, req.Namespace)
		if err := ctrl.SetControllerReference(&migratingPod, pod, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}

		// r.applyExcludeNodeSelector(&migratingPod, pod)
		pod.Finalizers = append(pod.Finalizers, migratingPodFinalizer)

		if err := r.Create(ctx, pod); err != nil {
			log.Error(err, "unable to create Pod for MigratingPod", "pod", pod)
			return ctrl.Result{}, err
		}

		migratingPod.Status.ActivePod = pod.Name
		if err := r.Update(ctx, &migratingPod); err != nil {
			log.Error(err, "failed to update status")
			return ctrl.Result{}, err
		}

		log.V(1).Info("created Pod for MigratingPod run", "pod", pod)
	case 1:
		curPod := childPods.Items[0]
		log.Info("Checking single pod")
		log = log.WithValues("current pod", curPod.Name)
		if !curPod.DeletionTimestamp.IsZero() {
			// Pod is getting deleted, let's migrate it
			newPod := GetPodFromTemplate(&migratingPod.Spec.Template, &migratingPod, req.Namespace)
			if err := ctrl.SetControllerReference(&migratingPod, newPod, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}

			// r.applyExcludeNodeSelector(&migratingPod, newPod)
			r.applyPodAntiAffinity(newPod, &curPod)
			newPod.Finalizers = append(newPod.Finalizers, migratingPodFinalizer)
			newPod.Spec.ClonePod = curPod.Name

			if err := r.Create(ctx, newPod); err != nil {
				log.Error(err, "unable to create Pod for MigratingPod", "pod", newPod)
				return ctrl.Result{}, err
			}
		}
	case 2:
		source := &childPods.Items[0]
		target := &childPods.Items[1]
		if source.Name != migratingPod.Status.ActivePod {
			tmp := source
			source = target
			target = tmp
		}
		log = log.WithValues("source pod", source.Name, "target pod", target.Name)

		if target.Status.Phase == corev1.PodRunning {
			migratingPod.Status.ActivePod = target.Name
			if err := r.Update(ctx, &migratingPod); err != nil {
				log.Error(err, "failed to update status")
				return ctrl.Result{}, err
			}

			for i, v := range source.Finalizers {
				if v == migratingPodFinalizer {
					source.Finalizers = RemoveString(source.Finalizers, i)
					break
				}
			}
			log.Info("removing finalizer", "finalizers", source.Finalizers)
			if err := r.Update(ctx, source); err != nil {
				log.Error(err, "failed to update source pod")
				return ctrl.Result{}, err
			}
		}

	default:
		// fail, as this cannot be? Or delete excess Pods? If yes, which?
		// Or just ignore since prototype -> failure
		err := errors.New("invalid state")
		log.Error(err, "more than 2 Pods returned for MigratingPod", "pod count", len(childPods.Items), "pods", childPods.Items)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *MigratingPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	// Index Pods so they're easier to query
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, podOwnerKey, func(raw runtime.Object) []string {
		pod := raw.(*corev1.Pod)
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil
		}
		if owner.Kind != "MigratingPod" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&podmigv1.MigratingPod{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

// func (r *MigratingPodReconciler) applyExcludeNodeSelector(migratingPod *podmigv1.MigratingPod, pod *corev1.Pod) {
// 	if migratingPod.Spec.ExcludeNodeSelector != nil {
// 		nodeSelectorRequirements := []corev1.NodeSelectorRequirement{}
// 		for k, v := range *migratingPod.Spec.ExcludeNodeSelector {
// 			nodeSelectorRequirements = append(nodeSelectorRequirements, corev1.NodeSelectorRequirement{
// 				Key:      k,
// 				Operator: corev1.NodeSelectorOpNotIn,
// 				Values:   []string{v},
// 			})
// 		}
// 		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(
// 			pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms,
// 			corev1.NodeSelectorTerm{
// 				MatchExpressions: nodeSelectorRequirements,
// 			})
// 	}
// }

func (r *MigratingPodReconciler) applyPodAntiAffinity(pod *corev1.Pod, antiPod *corev1.Pod) {
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.PodAntiAffinity == nil {
		pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}
	pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: antiPod.ObjectMeta.Labels,
		},
		TopologyKey: "kubernetes.io/hostname",
	})
}

func (r *MigratingPodReconciler) getChildPods(ctx context.Context, namespace, migPodName string) (*corev1.PodList, error) {
	var childPods corev1.PodList
	if err := r.List(ctx, &childPods, client.InNamespace(namespace), client.MatchingFields{podOwnerKey: migPodName}); err != nil {
		return nil, err
	}
	return &childPods, nil
}

func (r *MigratingPodReconciler) cleanUpOrphanPods(ctx context.Context, namespace, migPodName string) error {
	pods, err := r.getChildPods(ctx, namespace, migPodName)
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		changed := false
		for i, v := range p.Finalizers {
			if v == migratingPodFinalizer {
				p.Finalizers = RemoveString(p.Finalizers, i)
				changed = true
				break
			}
		}
		if changed {
			r.Log.Info("removing finalizer", "pod", p.Name)
			if err := r.Update(ctx, &p); err != nil {
				return err
			}
		}
	}
	return nil
}
