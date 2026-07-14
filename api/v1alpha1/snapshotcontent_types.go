package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
type SnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SnapshotContentSpec   `json:"spec,omitempty"`
	Status            SnapshotContentStatus `json:"status,omitempty"`
}

type SnapshotContentSpec struct {
	SnapshotHandle string `json:"snapshotHandle"`
}

type SnapshotContentStatus struct {
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotContent `json:"items"`
}
