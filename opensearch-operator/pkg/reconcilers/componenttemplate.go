package reconcilers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	opsterv1 "opensearch.opster.io/api/v1"
	"opensearch.opster.io/opensearch-gateway/services"
	"opensearch.opster.io/pkg/helpers"
	"opensearch.opster.io/pkg/reconcilers/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	opensearchComponentTemplateExists       = "component template already exists in OpenSearch; not modifying"
	opensearchComponentTemplateNameMismatch = "OpensearchComponentTemplateNameMismatch"
)

type ComponentTemplateReconciler struct {
	client.Client
	ReconcilerOptions
	ctx      context.Context
	osClient *services.OsClusterClient
	recorder record.EventRecorder
	instance *opsterv1.OpensearchComponentTemplate
	cluster  *opsterv1.OpenSearchCluster
	logger   logr.Logger
}

func NewComponentTemplateReconciler(
	ctx context.Context,
	client client.Client,
	recorder record.EventRecorder,
	instance *opsterv1.OpensearchComponentTemplate,
	opts ...ReconcilerOption,
) *ComponentTemplateReconciler {
	options := ReconcilerOptions{}
	options.apply(opts...)
	return &ComponentTemplateReconciler{
		Client:            client,
		ReconcilerOptions: options,
		ctx:               ctx,
		recorder:          recorder,
		instance:          instance,
		logger:            log.FromContext(ctx).WithValues("reconciler", "component template"),
	}
}

func (r *ComponentTemplateReconciler) Reconcile() (result ctrl.Result, err error) {
	var reason string

	defer func() {
		if !pointer.BoolDeref(r.updateStatus, true) {
			return
		}
		// When the reconciler is done, figure out what the state of the resource
		// is and set it in the state field accordingly.
		inErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(r.ctx, client.ObjectKeyFromObject(r.instance), r.instance); err != nil {
				return err
			}
			r.instance.Status.Reason = reason
			if err != nil {
				r.instance.Status.State = opsterv1.OpensearchComponentTemplateError
			}
			if result.Requeue && result.RequeueAfter == 10*time.Second {
				r.instance.Status.State = opsterv1.OpensearchComponentTemplatePending
			}
			if err == nil && result.RequeueAfter == 30*time.Second {
				r.instance.Status.State = opsterv1.OpensearchComponentTemplateCreated
			}
			if reason == opensearchComponentTemplateExists {
				r.instance.Status.State = opsterv1.OpensearchComponentTemplateIgnored
			}
			return r.Status().Update(r.ctx, r.instance)
		})

		if inErr != nil {
			r.logger.Error(inErr, "failed to update status")
		}
	}()

	r.cluster, err = util.FetchOpensearchCluster(r.ctx, r.Client, types.NamespacedName{
		Name:      r.instance.Spec.OpensearchRef.Name,
		Namespace: r.instance.Namespace,
	})
	if err != nil {
		reason = "error fetching opensearch cluster"
		r.logger.Error(err, "failed to fetch opensearch cluster")
		r.recorder.Event(r.instance, "Warning", opensearchError, reason)
		return
	}

	if r.cluster == nil {
		r.logger.Info("opensearch cluster does not exist, requeueing")
		reason = "waiting for opensearch cluster to exist"
		r.recorder.Event(r.instance, "Normal", opensearchPending, reason)
		result = ctrl.Result{
			Requeue:      true,
			RequeueAfter: 10 * time.Second,
		}
		return
	}

	// Check cluster ref has not changed
	if r.instance.Status.ManagedCluster != nil {
		if *r.instance.Status.ManagedCluster != r.cluster.UID {
			reason = "cannot change the cluster a component template refers to"
			err = fmt.Errorf("%s", reason)
			r.recorder.Event(r.instance, "Warning", opensearchRefMismatch, reason)
			return
		}
	} else {
		if pointer.BoolDeref(r.updateStatus, true) {
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Get(r.ctx, client.ObjectKeyFromObject(r.instance), r.instance); err != nil {
					return err
				}
				r.instance.Status.ManagedCluster = &r.cluster.UID
				return r.Status().Update(r.ctx, r.instance)
			})
			if err != nil {
				reason = fmt.Sprintf("failed to update status: %s", err)
				r.recorder.Event(r.instance, "Warning", statusError, reason)
				return
			}
		}
	}

	// Check cluster is ready
	if r.cluster.Status.Phase != opsterv1.PhaseRunning {
		r.logger.Info("opensearch cluster is not running, requeueing")
		reason = "waiting for opensearch cluster status to be running"
		r.recorder.Event(r.instance, "Normal", opensearchPending, reason)
		result = ctrl.Result{
			Requeue:      true,
			RequeueAfter: 10 * time.Second,
		}
		return
	}

	r.osClient, err = util.CreateClientForCluster(r.ctx, r.Client, r.cluster, r.osClientTransport)
	if err != nil {
		reason = "error creating opensearch client"
		r.recorder.Event(r.instance, "Warning", opensearchError, reason)
		return
	}

	templateName := r.instance.Name
	if r.instance.Spec.Name != "" {
		templateName = r.instance.Spec.Name
	}

	// Check component template state to make sure we don't touch preexisting component templates
	if r.instance.Status.ExistingComponentTemplate == nil {
		var exists bool
		exists, err = services.ComponentTemplateExists(r.ctx, r.osClient, templateName)
		if err != nil {
			reason = "failed to get component template status from OpenSearch API"
			r.logger.Error(err, reason)
			r.recorder.Event(r.instance, "Warning", opensearchAPIError, reason)
			return
		}
		if pointer.BoolDeref(r.updateStatus, true) {
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Get(r.ctx, client.ObjectKeyFromObject(r.instance), r.instance); err != nil {
					return err
				}
				r.instance.Status.ExistingComponentTemplate = &exists
				return r.Status().Update(r.ctx, r.instance)
			})
			if err != nil {
				reason = fmt.Sprintf("failed to update status: %s", err)
				r.recorder.Event(r.instance, "Warning", statusError, reason)
				return
			}
		} else {
			// Emit an event for unit testing assertion
			r.recorder.Event(r.instance, "Normal", "UnitTest", fmt.Sprintf("exists is %t", exists))
			return
		}
	}

	// If component template is existing do nothing
	if *r.instance.Status.ExistingComponentTemplate {
		reason = opensearchComponentTemplateExists
		return
	}

	// the template name is immutable, so check the old name (r.instance.Status.ComponentTemplateName) against the new
	if r.instance.Status.ComponentTemplateName != "" && templateName != r.instance.Status.ComponentTemplateName {
		reason = "cannot change the component template name"
		err = fmt.Errorf("%s", reason)
		r.recorder.Event(r.instance, "Warning", opensearchComponentTemplateNameMismatch, reason)
		return
	}

	// rewrite the CRD format to the gateway format
	resource := helpers.TranslateComponentTemplateToRequest(r.instance.Spec)

	shouldUpdate, err := services.ShouldUpdateComponentTemplate(r.ctx, r.osClient, templateName, resource)
	if err != nil {
		reason = "failed to get component template status from OpenSearch API"
		r.logger.Error(err, reason)
		r.recorder.Event(r.instance, "Warning", opensearchAPIError, reason)
		return
	}

	if !shouldUpdate {
		r.logger.V(1).Info(fmt.Sprintf("component template %s is in sync", r.instance.Name))
		result = ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}
		return
	}

	err = services.CreateOrUpdateComponentTemplate(r.ctx, r.osClient, templateName, resource)
	if err != nil {
		reason = "failed to update component template with OpenSearch API"
		r.logger.Error(err, reason)
		r.recorder.Event(r.instance, "Warning", opensearchAPIError, reason)
	}

	r.recorder.Event(r.instance, "Normal", opensearchAPIUpdated, "component template updated in opensearch")

	result = ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}
	return
}

func (r *ComponentTemplateReconciler) Delete() error {
	// If we have never successfully reconciled we can just exit
	if r.instance.Status.ExistingComponentTemplate == nil {
		return nil
	}

	if *r.instance.Status.ExistingComponentTemplate {
		r.logger.Info("component template was pre-existing; not deleting")
		return nil
	}

	var err error

	r.cluster, err = util.FetchOpensearchCluster(r.ctx, r.Client, types.NamespacedName{
		Name:      r.instance.Spec.OpensearchRef.Name,
		Namespace: r.instance.Namespace,
	})
	if err != nil {
		return err
	}

	if r.cluster == nil || !r.cluster.DeletionTimestamp.IsZero() {
		// If the opensearch cluster doesn't exist, we don't need to delete anything
		return nil
	}

	r.osClient, err = util.CreateClientForCluster(r.ctx, r.Client, r.cluster, r.osClientTransport)
	if err != nil {
		return err
	}

	templateName := r.instance.Name
	if r.instance.Spec.Name != "" {
		templateName = r.instance.Spec.Name
	}

	exist, err := services.ComponentTemplateExists(r.ctx, r.osClient, templateName)
	if err != nil {
		return err
	}
	if !exist {
		r.logger.V(1).Info("component template already deleted from opensearch")
		return nil
	}

	return services.DeleteComponentTemplate(r.ctx, r.osClient, templateName)
}
