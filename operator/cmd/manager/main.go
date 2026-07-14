package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func main() {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to register API types")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}
}
