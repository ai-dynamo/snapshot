package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var SchemeGroupVersion = schema.GroupVersion{Group: "nvidia.com", Version: "v1alpha1"}

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&Snapshot{}, &SnapshotList{},
		&SnapshotContent{}, &SnapshotContentList{},
		&SnapshotJob{}, &SnapshotJobList{},
	)
	return nil
}
