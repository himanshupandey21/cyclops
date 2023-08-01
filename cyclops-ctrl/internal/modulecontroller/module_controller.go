/*
Copyright 2023.

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

package modulecontroller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cyclopsv1alpha1 "github.com/cyclops-ui/cycops-ctrl/api/v1alpha1"
	"github.com/cyclops-ui/cycops-ctrl/internal/cluster/k8sclient"
	"github.com/cyclops-ui/cycops-ctrl/internal/models"
	"github.com/cyclops-ui/cycops-ctrl/internal/storage/templates"
	template2 "github.com/cyclops-ui/cycops-ctrl/internal/template"
)

// ModuleReconciler reconciles a Module object
type ModuleReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	templates        *templates.Storage
	kubernetesClient *k8sclient.KubernetesClient

	logger logr.Logger
}

func NewModuleReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	templates *templates.Storage,
	kubernetesClient *k8sclient.KubernetesClient,
) *ModuleReconciler {
	return &ModuleReconciler{
		Client:           client,
		Scheme:           scheme,
		templates:        templates,
		kubernetesClient: kubernetesClient,
		logger:           ctrl.Log.WithName("reconciler"),
	}
}

//+kubebuilder:rbac:groups=cyclops-ui.com,resources=modules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cyclops-ui.com,resources=modules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cyclops-ui.com,resources=modules/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Module object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *ModuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var module cyclopsv1alpha1.Module
	err := r.Get(ctx, req.NamespacedName, &module)
	if apierrors.IsNotFound(err) {
		r.logger.Info("delete module", "namespaced name", req.NamespacedName)
		resources, err := r.kubernetesClient.GetResourcesForModule(req.Name)
		if err != nil {
			r.logger.Error(err, "error on get module resources", "namespaced name", req.NamespacedName)
			return ctrl.Result{}, err
		}

		for _, resource := range resources {
			if err := r.kubernetesClient.Delete(resource); err != nil {
				r.logger.Error(
					err,
					"error on delete module while deleting resource",
					"module namespaced name",
					req.NamespacedName,
					"resource namespaced name",
					fmt.Sprintf("%s/%s", resource.GetNamespace(), resource.GetName()),
				)
			}
		}

		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	r.logger.Info("upsert module", "namespaced name", req.NamespacedName)
	if err := r.moduleToResources(req.Name); err != nil {
		r.logger.Error(err, "error on upsert module", "namespaced name", req.NamespacedName)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cyclopsv1alpha1.Module{}).
		Complete(r)
}

func (r *ModuleReconciler) moduleToResources(name string) error {
	module, err := r.kubernetesClient.GetModule(name)
	if err != nil {
		return err
	}

	template, err := r.templates.GetConfig(module.Spec.TemplateRef)
	if err != nil {
		return err
	}

	if err := generateResources(r.kubernetesClient, *module, template); err != nil {
		return err
	}

	return nil
}

func generateResources(kClient *k8sclient.KubernetesClient, module cyclopsv1alpha1.Module, moduleTemplate models.Template) error {
	out, err := template2.HelmTemplate(module, moduleTemplate)
	if err != nil {
		return err
	}

	objects := make([]runtime.Object, 0, 0)

	for _, s := range strings.Split(out, "---") {
		s := strings.TrimSpace(s)
		if len(s) == 0 {
			continue
		}

		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(s), nil, nil)
		if err != nil {
			return err
		}

		objects = append(objects, obj)
	}

	for _, object := range objects {
		switch rs := object.(type) {
		case *appsv1.Deployment:
			labels := rs.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}

			labels["cyclops.module"] = module.Name
			rs.SetLabels(labels)

			if err := kClient.Deploy(rs); err != nil {
				return err
			}
		case *v1.Service:
			labels := rs.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}

			labels["cyclops.module"] = module.Name
			rs.SetLabels(labels)

			if err := kClient.DeployService(rs); err != nil {
				return err
			}
		}
	}

	return nil
}