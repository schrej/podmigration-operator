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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigratingPodSpec defines the desired state of MigratingPod
type MigratingPodSpec struct {
	// Template describes the pods that will be created.
	// +kubebuilder:validation:Required
	Template corev1.PodTemplateSpec `json:"template"`

	// ExcludeNode indicates a node that the Pod should not get scheduled on or get migrated
	// away from.
	// +kubebuilder:validation:Optional
	// ExcludeNodeSelector map[string]string `json:"excludeNodeSelector"`
}

// MigratingPodStatus defines the observed state of MigratingPod
type MigratingPodStatus struct {
	// State indicates the state of the MigratingPod
	State string `json:"state"`

	// CurrentRevision indicates the version of the MigratingPod to generate the current Pod
	CurrentRevision string `json:"currentRevision"`

	// ActivePod
	ActivePod string `json:"activePod"`
}

// +kubebuilder:object:root=true

// MigratingPod is the Schema for the migratingpods API
type MigratingPod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MigratingPodSpec   `json:"spec,omitempty"`
	Status MigratingPodStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MigratingPodList contains a list of MigratingPod
type MigratingPodList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MigratingPod `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MigratingPod{}, &MigratingPodList{})
}
