package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SnapshotJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SnapshotJobSpec   `json:"spec,omitempty"`
	Status            SnapshotJobStatus `json:"status,omitempty"`
}

type SnapshotJobSpec struct {
	SnapshotName string `json:"snapshotName"`
}

type SnapshotJobStatus struct {
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
type SnapshotJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotJob `json:"items"`
}
