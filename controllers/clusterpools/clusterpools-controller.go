// Copyright Contributors to the Open Cluster Management project.

package clusterpools

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const DEBUG = 1
const INFO = 0
const WARN = -1
const ERROR = -2
const FINALIZER = "clusterpools-controller.open-cluster-management.io/cleanup"

const LABEL_NAMESPACE = "open-cluster-management.io/managed-by"
const CLUSTERPOOLS = "clusterpools"

// ClusterPoolsReconciler reconciles a ClusterPool, mainly for the delete
type ClusterPoolsReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

func (r *ClusterPoolsReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {

	ctx := context.Background()

	log := r.Log.WithValues("ClusterPoolsReconciler", req.NamespacedName)

	var cp hivev1.ClusterPool
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		log.V(INFO).Info("Resource deleted")

		return ctrl.Result{}, nil
	}

	// Early exit
	if cp.DeletionTimestamp == nil && controllerutil.ContainsFinalizer(&cp, FINALIZER) {
		return ctrl.Result{}, nil
	}

	target := cp.Name
	log.V(INFO).Info("Reconcile cluster pool: " + target)

	if cp.DeletionTimestamp != nil {
		if err := deleteResources(r, &cp); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, removeFinalizer(r, &cp)
	}

	return ctrl.Result{}, setFinalizer(r, &cp)
}

func (r *ClusterPoolsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hivev1.ClusterPool{}).WithEventFilter(predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}).WithOptions(controller.Options{
		MaxConcurrentReconciles: 1, // This is the default
	}).Complete(r)
}

func setFinalizer(r *ClusterPoolsReconciler, cc *hivev1.ClusterPool) error {

	patch := client.MergeFrom(cc.DeepCopy())

	controllerutil.AddFinalizer(cc, FINALIZER)

	return r.Patch(context.Background(), cc, patch)
}

func removeFinalizer(r *ClusterPoolsReconciler, cc *hivev1.ClusterPool) error {

	if !controllerutil.ContainsFinalizer(cc, FINALIZER) {
		return nil
	}

	controllerutil.RemoveFinalizer(cc, FINALIZER)

	err := r.Update(context.Background(), cc)
	if err == nil {
		r.Log.V(INFO).Info("Removed finalizer on cluster pool: " + cc.Name)
	}
	return err

}

func deleteResources(r *ClusterPoolsReconciler, cp *hivev1.ClusterPool) error {
	ctx := context.Background()
	log := r.Log

	var cps hivev1.ClusterPoolList
	if err := r.List(ctx, &cps, &client.ListOptions{Namespace: cp.Namespace}); err != nil {

		if k8serrors.IsNotFound(err) {
			log.V(INFO).Info("No Cluster Pools found")
			return nil
		} else {
			return err
		}

	} else {

		// Remove secrets that are not used by any other cluster pool in the namespace
		foundPullSecret := false
		foundInstallConfigSecret := false
		foundProviderSecret := false
		providerSecretName := ""

		cpType := "skip"
		if cp.Spec.Platform.AWS != nil {
			cpType = "aws"
			providerSecretName = cp.Spec.Platform.AWS.CredentialsSecretRef.Name
		} else if cp.Spec.Platform.GCP != nil {
			cpType = "gcp"
			providerSecretName = cp.Spec.Platform.GCP.CredentialsSecretRef.Name
		} else if cp.Spec.Platform.Azure != nil {
			cpType = "azure"
			providerSecretName = cp.Spec.Platform.Azure.CredentialsSecretRef.Name
		}

		for _, foundCp := range cps.Items {
			if cp.Name == foundCp.Name {
				continue
			}
			if cp.Spec.PullSecretRef.Name == foundCp.Spec.PullSecretRef.Name {
				foundPullSecret = true
			}

			if cp.Spec.InstallConfigSecretTemplateRef.Name == foundCp.Spec.InstallConfigSecretTemplateRef.Name {
				foundInstallConfigSecret = true
			}

			// This needs to happen after the cp.Name == foundCp.Name check
			switch cpType {
			case "aws":
				if foundCp.Spec.Platform.AWS != nil {
					if cp.Spec.Platform.AWS.CredentialsSecretRef.Name == foundCp.Spec.Platform.AWS.CredentialsSecretRef.Name {
						foundProviderSecret = true
					}
				}
			case "gcp":
				if foundCp.Spec.Platform.GCP != nil {
					if cp.Spec.Platform.GCP.CredentialsSecretRef.Name == foundCp.Spec.Platform.GCP.CredentialsSecretRef.Name {
						foundProviderSecret = true
					}
				}
			case "azure":
				if foundCp.Spec.Platform.Azure != nil {
					if cp.Spec.Platform.Azure.CredentialsSecretRef.Name == foundCp.Spec.Platform.Azure.CredentialsSecretRef.Name {
						foundProviderSecret = true
					}
				}
			}
		}

		log.V(INFO).Info(fmt.Sprintf("Secrets found, install-config: %v, Pull secret: %v, Provider credential: %v", foundInstallConfigSecret, foundPullSecret, foundProviderSecret))
		log.V(DEBUG).Info(fmt.Sprintf("providerSecretName: %v", providerSecretName))
		var secret corev1.Secret

		if !foundInstallConfigSecret {

			err := r.Get(ctx, types.NamespacedName{Name: cp.Spec.InstallConfigSecretTemplateRef.Name, Namespace: cp.Namespace}, &secret)
			if err == nil {
				err := r.Delete(ctx, &secret)
				if err != nil {
					return err
				}
				log.V(INFO).Info("Deleted install-config secret: " + secret.Name)
			}
		}

		if !foundPullSecret {

			err := r.Get(ctx, types.NamespacedName{Name: cp.Spec.PullSecretRef.Name, Namespace: cp.Namespace}, &secret)
			if err == nil {
				err := r.Delete(ctx, &secret)
				if err != nil {
					return err
				}
				log.V(INFO).Info("Deleted pull secret: " + secret.Name)
			}
		}

		if !foundProviderSecret && providerSecretName != "" {

			err := r.Get(ctx, types.NamespacedName{Name: providerSecretName, Namespace: cp.Namespace}, &secret)
			if err == nil {
				err := r.Delete(ctx, &secret)
				if err != nil {
					return err
				}
				log.V(INFO).Info("Deleted provider credential secret: " + secret.Name)
			}
		}
	}

	// Remove the namespace if only the deleted ClusterPool was found
	log.V(INFO).Info(fmt.Sprintf("Cluster Pools found in namespace: %v", len(cps.Items)))
	if len(cps.Items) == 1 {
		var ns corev1.Namespace
		err := r.Get(ctx, types.NamespacedName{Name: cp.Namespace}, &ns)
		if err == nil {
			if ns.Labels != nil && ns.Labels[LABEL_NAMESPACE] == CLUSTERPOOLS {

				ns := &corev1.Namespace{ObjectMeta: v1.ObjectMeta{Name: cp.Namespace}}
				err := r.Delete(ctx, ns)
				if err != nil {
					return err
				}

				log.V(INFO).Info("Deleted namespace: " + ns.Name)
			} else {
				log.V(INFO).Info("Did not delete namespace: " + ns.Name + " it is still in use")
			}
		}
	}

	return nil
}