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
	"strconv"
	"strings"
	"time"

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
	}

	childPods, err := r.getChildPods(ctx, req.Namespace, req.Name)
	if err != nil {
		log.Error(err, "unable to list child Pods")
		return ctrl.Result{}, err
	}

	if migratingPod.Name == "" || migratingPod.DeletionTimestamp != nil {
		log.Info("MigratingPod deleted, cleaning up orphan pods.")
		err := r.cleanUpOrphanPods(ctx, childPods)
		return ctrl.Result{}, err
	}

	for _, pod := range childPods.Items {
		log.Info("found pod", "name", pod.Name, "owner", pod.ObjectMeta.OwnerReferences[0].Name)
	}

	source, target := r.getSourceTargetPods(migratingPod.Status, childPods)
	if source != nil {
		log = log.WithValues("source", source.Name)
	}
	if target != nil {
		log = log.WithValues("target", target.Name)
	}
	// if there are pods, but we can't find a source pod, the state is somewhat invalid
	// this can happen if a new migration is triggered before the old source Pod is fully deleted
	if source == nil && len(childPods.Items) > 0 {
		// todo: find a better strategy here
		err := errors.New("invalid state")
		log.Error(err, "couldn't determine source pod", "pod count", len(childPods.Items))
		migratingPod.Status.State = podmigv1.StateInvalid
		if err := r.Update(ctx, &migratingPod); err != nil {
			log.Error(err, "failed to update status of invalid pod")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	// update MigratingPod status
	log.Info("updating status")

	if target != nil {
		if target.Status.Phase == corev1.PodRunning {
			// once the target is running, it becomes the active Pod
			migratingPod.Status.ActivePod = target.Name
			migratingPod.Status.State = podmigv1.StateRunning
		} else {
			// if it's not running, we're migrating
			migratingPod.Status.State = podmigv1.StateMigrating
		}
	} else if source != nil {
		if !source.DeletionTimestamp.IsZero() {
			migratingPod.Status.State = podmigv1.StateMigrationPending
		} else if source.Status.Phase == corev1.PodRunning {
			migratingPod.Status.ActivePod = source.Name
			migratingPod.Status.State = podmigv1.StateRunning
		}
	}
	if err := r.Update(ctx, &migratingPod); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	// synchronize MigratingPod
	log.Info("synchronizing")

	if source == nil {
		// if no pod exists, create it
		// this should always happen, the check is just a precaution
		if len(childPods.Items) == 0 {
			log.Info("creating initial pod")
			pod := GetPodFromTemplate(&migratingPod.Spec.Template, &migratingPod, req.Namespace)
			if err := ctrl.SetControllerReference(&migratingPod, pod, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}
			pod.Name = GetFirstPodName(&migratingPod)

			// r.applyExcludeNodeSelector(&migratingPod, pod)
			pod.Finalizers = append(pod.Finalizers, migratingPodFinalizer)

			if err := r.Create(ctx, pod); err != nil {
				log.Error(err, "unable to create Pod for MigratingPod", "pod", pod)
				return ctrl.Result{}, err
			}

			if err := r.Update(ctx, &migratingPod); err != nil {
				log.Error(err, "failed to update status")
				return ctrl.Result{}, err
			}

			log.V(1).Info("created pod", "pod", pod.Name)
			return ctrl.Result{}, nil
		}
	} else {
		// if source pod is deleted and no longer the active pod, remove finalizer
		if !source.DeletionTimestamp.IsZero() && migratingPod.Status.ActivePod != source.Name {
			log.Info("removing finalizer of source pod")
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

		// if source Pod is getting deleted, migrate it
		if target == nil && !source.DeletionTimestamp.IsZero() {
			log.Info("migrating pod due to soure pod deletion")
			target := GetPodFromTemplate(&migratingPod.Spec.Template, &migratingPod, req.Namespace)
			if err := ctrl.SetControllerReference(&migratingPod, target, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}
			target.Name = r.getNextPodName(source.Name)

			// r.applyExcludeNodeSelector(&migratingPod, newPod)
			r.applyPodAntiAffinity(target, source)
			target.Finalizers = append(target.Finalizers, migratingPodFinalizer)
			target.Spec.ClonePod = source.Name

			if err := r.Create(ctx, target); err != nil {
				log.Error(err, "unable to create Pod for MigratingPod", "pod", target.Name)
				return ctrl.Result{}, err
			}

			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
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

func (r *MigratingPodReconciler) cleanUpOrphanPods(ctx context.Context, pods *corev1.PodList) error {
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

// getSourceTargetPods returns the source and target Pod from a list of childPods based on their creation timestamp.
// If the list only contains one Pod, it is returned as the source pod. If the list contains zero or more than two
// Pods, nil is returned for source and target.
func (r *MigratingPodReconciler) getSourceTargetPods(status podmigv1.MigratingPodStatus, childPods *corev1.PodList) (source, target *corev1.Pod) {
	if len(childPods.Items) == 1 {
		return &childPods.Items[0], nil
	}
	if len(childPods.Items) == 2 {
		if childPods.Items[0].CreationTimestamp.Before(&childPods.Items[1].CreationTimestamp) {
			return &childPods.Items[0], &childPods.Items[1]
		} else {
			return &childPods.Items[1], &childPods.Items[0]
		}
	}
	return nil, nil
}

func (r *MigratingPodReconciler) getNextPodName(curName string) string {
	i := strings.LastIndex(curName, "-")
	n, err := strconv.Atoi(curName[i+1:])
	if err != nil {
		return curName[:i-1]
	}
	return curName[:i+1] + strconv.Itoa(n+1)
}
