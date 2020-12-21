package controllers

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

func GetPodFromTemplate(template *corev1.PodTemplateSpec, parentObject runtime.Object, namespace string) *corev1.Pod {
	desiredLabels := getPodsLabelSet(template)
	desiredFinalizers := getPodsFinalizers(template)
	desiredAnnotations := getPodsAnnotationSet(template)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Labels:      desiredLabels,
			Finalizers:  desiredFinalizers,
			Annotations: desiredAnnotations,
		},
	}
	pod.Spec = *template.Spec.DeepCopy()
	return pod
}

func getPodsLabelSet(template *corev1.PodTemplateSpec) labels.Set {
	desiredLabels := make(labels.Set)
	for k, v := range template.Labels {
		desiredLabels[k] = v
	}
	return desiredLabels
}

func getPodsFinalizers(template *corev1.PodTemplateSpec) []string {
	desiredFinalizers := make([]string, len(template.Finalizers))
	copy(desiredFinalizers, template.Finalizers)
	return desiredFinalizers
}

func getPodsAnnotationSet(template *corev1.PodTemplateSpec) labels.Set {
	desiredAnnotations := make(labels.Set)
	for k, v := range template.Annotations {
		desiredAnnotations[k] = v
	}
	return desiredAnnotations
}

func GetFirstPodName(parentObject runtime.Object) string {
	accessor, _ := meta.Accessor(parentObject)
	name := fmt.Sprintf("%s-0", accessor.GetName())
	return name
}

// RemoveString removes the element at position i from a string array without preserving
// the order.
// https://stackoverflow.com/a/37335777/4430124
func RemoveString(s []string, i int) []string {
	s[i] = s[len(s)-1]
	return s[:len(s)-1]
}
