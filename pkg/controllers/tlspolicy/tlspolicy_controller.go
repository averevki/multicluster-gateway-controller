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

package tlspolicy

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayapiv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/kuadrant/kuadrant-operator/pkg/common"
	"github.com/kuadrant/kuadrant-operator/pkg/reconcilers"

	"github.com/Kuadrant/multicluster-gateway-controller/pkg/_internal/conditions"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/controllers/events"
)

const (
	TLSPolicyFinalizer                                    = "kuadrant.io/tls-policy"
	TLSPoliciesBackRefAnnotation                          = "kuadrant.io/tlspolicies"
	TLSPolicyBackRefAnnotation                            = "kuadrant.io/tlspolicy"
	TLSPolicyAffected            conditions.ConditionType = "kuadrant.io/TLSPolicyAffected"
)

type TLSPolicyRefsConfig struct{}

func (c *TLSPolicyRefsConfig) PolicyRefsAnnotation() string {
	return TLSPoliciesBackRefAnnotation
}

// TLSPolicyReconciler reconciles a TLSPolicy object
type TLSPolicyReconciler struct {
	reconcilers.TargetRefReconciler
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=kuadrant.io,resources=tlspolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kuadrant.io,resources=tlspolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kuadrant.io,resources=tlspolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups="cert-manager.io",resources=issuers,verbs=get;list;
//+kubebuilder:rbac:groups="cert-manager.io",resources=clusterissuers,verbs=get;list;

func (r *TLSPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger().WithValues("TLSPolicy", req.NamespacedName)
	log.Info("Reconciling TLSPolicy")
	ctx = crlog.IntoContext(ctx, log)

	previous := &v1alpha1.TLSPolicy{}
	if err := r.Client().Get(ctx, req.NamespacedName, previous); err != nil {
		if err := client.IgnoreNotFound(err); err == nil {
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	tlsPolicy := previous.DeepCopy()
	log.V(3).Info("TLSPolicyReconciler Reconcile", "tlsPolicy", tlsPolicy, "tlsPolicy.Spec", tlsPolicy.Spec)

	markedForDeletion := tlsPolicy.GetDeletionTimestamp() != nil

	targetNetworkObject, err := r.FetchValidTargetRef(ctx, tlsPolicy.GetTargetRef(), tlsPolicy.Namespace)
	log.V(3).Info("TLSPolicyReconciler targetNetworkObject", "targetNetworkObject", targetNetworkObject)
	if err != nil {
		if !markedForDeletion {
			if apierrors.IsNotFound(err) {
				log.V(3).Info("Network object not found. Cleaning up")
				delResErr := r.deleteResources(ctx, tlsPolicy, nil)
				if delResErr == nil {
					delResErr = err
				}
				return r.reconcileStatus(ctx, tlsPolicy, fmt.Errorf("%w: %w", conditions.ErrTargetNotFound, delResErr))

			}
			return ctrl.Result{}, err
		}
		targetNetworkObject = nil // we need the object set to nil when there's an error, otherwise deleting the resources (when marked for deletion) will panic
	}

	if markedForDeletion {
		log.V(3).Info("cleaning up tls policy")
		if controllerutil.ContainsFinalizer(tlsPolicy, TLSPolicyFinalizer) {
			if err := r.deleteResources(ctx, tlsPolicy, targetNetworkObject); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.RemoveFinalizer(ctx, tlsPolicy, TLSPolicyFinalizer); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// add finalizer to the tlsPolicy
	if !controllerutil.ContainsFinalizer(tlsPolicy, TLSPolicyFinalizer) {
		if err := r.AddFinalizer(ctx, tlsPolicy, TLSPolicyFinalizer); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}

	specErr := r.reconcileResources(ctx, tlsPolicy, targetNetworkObject)

	statusResult, statusErr := r.reconcileStatus(ctx, tlsPolicy, specErr)

	if specErr != nil {
		return ctrl.Result{}, specErr
	}

	return statusResult, statusErr
}

func (r *TLSPolicyReconciler) reconcileResources(ctx context.Context, tlsPolicy *v1alpha1.TLSPolicy, targetNetworkObject client.Object) error {
	gatewayCondition := conditions.BuildPolicyAffectedCondition(TLSPolicyAffected, tlsPolicy, targetNetworkObject, conditions.PolicyReasonAccepted, nil)

	// validate
	err := tlsPolicy.Validate()
	if err != nil {
		return err
	}

	err = validateIssuer(ctx, r.Client(), tlsPolicy)
	if err != nil {
		return err
	}

	// reconcile based on gateway diffs
	gatewayDiffObj, err := r.ComputeGatewayDiffs(ctx, tlsPolicy, targetNetworkObject, &TLSPolicyRefsConfig{})
	if err != nil {
		return err
	}

	if err = r.reconcileCertificates(ctx, tlsPolicy, gatewayDiffObj); err != nil {
		gatewayCondition = conditions.BuildPolicyAffectedCondition(TLSPolicyAffected, tlsPolicy, targetNetworkObject, conditions.PolicyReasonInvalid, err)
		updateErr := r.updateGatewayCondition(ctx, gatewayCondition, gatewayDiffObj)
		return errors.Join(fmt.Errorf("reconcile Certificates error %w", err), updateErr)
	}

	// set direct back ref - i.e. claim the target network object as taken asap
	if err = r.ReconcileTargetBackReference(ctx, client.ObjectKeyFromObject(tlsPolicy), targetNetworkObject, TLSPolicyBackRefAnnotation); err != nil {
		gatewayCondition = conditions.BuildPolicyAffectedCondition(TLSPolicyAffected, tlsPolicy, targetNetworkObject, conditions.PolicyReasonConflicted, err)
		updateErr := r.updateGatewayCondition(ctx, gatewayCondition, gatewayDiffObj)
		return errors.Join(fmt.Errorf("reconcile TargetBackReference error %w", err), updateErr)
	}

	// set annotation of policies affecting the gateway
	if err = r.ReconcileGatewayPolicyReferences(ctx, tlsPolicy, gatewayDiffObj); err != nil {
		gatewayCondition = conditions.BuildPolicyAffectedCondition(TLSPolicyAffected, tlsPolicy, targetNetworkObject, conditions.PolicyReasonUnknown, err)
		updateErr := r.updateGatewayCondition(ctx, gatewayCondition, gatewayDiffObj)
		return errors.Join(fmt.Errorf("ReconcileGatewayPolicyReferences error %w", err), updateErr)
	}

	// set gateway policy affected condition status - should be the last step, only when all the reconciliation steps succeed
	updateErr := r.updateGatewayCondition(ctx, gatewayCondition, gatewayDiffObj)
	if updateErr != nil {
		return fmt.Errorf("failed to update gateway conditions %w ", updateErr)
	}

	return nil
}

func (r *TLSPolicyReconciler) deleteResources(ctx context.Context, tlsPolicy *v1alpha1.TLSPolicy, targetNetworkObject client.Object) error {
	// delete based on gateway diffs

	gatewayDiffObj, err := r.ComputeGatewayDiffs(ctx, tlsPolicy, targetNetworkObject, &TLSPolicyRefsConfig{})
	if err != nil {
		return err
	}

	if err := r.deleteCertificates(ctx, tlsPolicy); err != nil {
		return err
	}

	// remove direct back ref
	if targetNetworkObject != nil {
		if err := r.DeleteTargetBackReference(ctx, client.ObjectKeyFromObject(tlsPolicy), targetNetworkObject, TLSPolicyBackRefAnnotation); err != nil {
			return err
		}
	}

	// update annotation of policies affecting the gateway
	if err := r.ReconcileGatewayPolicyReferences(ctx, tlsPolicy, gatewayDiffObj); err != nil {
		return err
	}

	// remove gateway policy affected condition status
	return r.updateGatewayCondition(ctx, metav1.Condition{Type: string(TLSPolicyAffected)}, gatewayDiffObj)
}

func (r *TLSPolicyReconciler) reconcileStatus(ctx context.Context, tlsPolicy *v1alpha1.TLSPolicy, specErr error) (ctrl.Result, error) {
	newStatus := r.calculateStatus(tlsPolicy, specErr)

	if !equality.Semantic.DeepEqual(newStatus, tlsPolicy.Status) {
		tlsPolicy.Status = *newStatus
		updateErr := r.Client().Status().Update(ctx, tlsPolicy)
		if updateErr != nil {
			// Ignore conflicts, resource might just be outdated.
			if apierrors.IsConflict(updateErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, updateErr
		}
	}

	if errors.Is(specErr, conditions.ErrTargetNotFound) {
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

func (r *TLSPolicyReconciler) calculateStatus(tlsPolicy *v1alpha1.TLSPolicy, specErr error) *v1alpha1.TLSPolicyStatus {
	newStatus := tlsPolicy.Status.DeepCopy()
	if specErr != nil {
		newStatus.ObservedGeneration = tlsPolicy.Generation
	}
	readyCond := r.readyCondition(string(tlsPolicy.Spec.TargetRef.Kind), specErr)
	meta.SetStatusCondition(&newStatus.Conditions, *readyCond)
	return newStatus
}

func (r *TLSPolicyReconciler) readyCondition(targetNetworkObjectectKind string, specErr error) *metav1.Condition {
	cond := &metav1.Condition{
		Type:    string(conditions.ConditionTypeReady),
		Status:  metav1.ConditionTrue,
		Reason:  fmt.Sprintf("%sTLSEnabled", targetNetworkObjectectKind),
		Message: fmt.Sprintf("%s is TLS Enabled", targetNetworkObjectectKind),
	}

	if specErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Message = specErr.Error()
		cond.Reason = "ReconciliationError"

		if errors.Is(specErr, conditions.ErrTargetNotFound) {
			cond.Reason = string(conditions.PolicyReasonTargetNotFound)
		}
	}

	return cond
}

func (r *TLSPolicyReconciler) updateGatewayCondition(ctx context.Context, condition metav1.Condition, gatewayDiff *reconcilers.GatewayDiff) error {

	// update condition if needed
	for _, gw := range append(gatewayDiff.GatewaysWithValidPolicyRef, gatewayDiff.GatewaysMissingPolicyRef...) {
		previous := gw.DeepCopy()
		meta.SetStatusCondition(&gw.Status.Conditions, condition)
		if !reflect.DeepEqual(previous.Status.Conditions, gw.Status.Conditions) {
			if err := r.Client().Status().Update(ctx, gw.Gateway); err != nil {
				return err
			}
		}
	}

	// remove condition from gateway that is no longer referenced
	for _, gw := range gatewayDiff.GatewaysWithInvalidPolicyRef {
		previous := gw.DeepCopy()
		meta.RemoveStatusCondition(&gw.Status.Conditions, condition.Type)
		if !reflect.DeepEqual(previous.Status.Conditions, gw.Status.Conditions) {
			if err := r.Client().Status().Update(ctx, gw.Gateway); err != nil {
				return err
			}
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TLSPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gatewayEventMapper := events.NewGatewayEventMapper(r.Logger(), &TLSPolicyRefsConfig{}, "tlspolicy")
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.TLSPolicy{}).
		Watches(
			&source.Kind{Type: &gatewayapiv1beta1.Gateway{}},
			handler.EnqueueRequestsFromMapFunc(gatewayEventMapper.MapToPolicy),
		).
		Complete(r)
}

// The following methods are here temporarily and copied from the kuadrant-operator https://github.com/Kuadrant/kuadrant-operator/blob/main/pkg/reconcilers/targetref_reconciler.go#L45
// FetchValidTargetRef and FetchValidGateway currently expect that the gateway should have a ready condition, but in the
// case of the TLSPolicy it might not be ready because there is an invalid tls section that this policy would resolve.
// ToDo mnairn: Create issue in kuadrant-operator and link it here

// FetchValidTargetRef fetches the target reference object and checks the status is valid
func (r *TLSPolicyReconciler) FetchValidTargetRef(ctx context.Context, targetRef gatewayapiv1alpha2.PolicyTargetReference, defaultNs string) (client.Object, error) {
	tmpNS := defaultNs
	if targetRef.Namespace != nil {
		tmpNS = string(*targetRef.Namespace)
	}

	objKey := client.ObjectKey{Name: string(targetRef.Name), Namespace: tmpNS}

	if common.IsTargetRefHTTPRoute(targetRef) {
		return r.FetchValidHTTPRoute(ctx, objKey)
	} else if common.IsTargetRefGateway(targetRef) {
		return r.FetchValidGateway(ctx, objKey)
	}

	return nil, fmt.Errorf("FetchValidTargetRef: targetRef (%v) to unknown network resource", targetRef)
}

func (r *TLSPolicyReconciler) FetchValidGateway(ctx context.Context, key client.ObjectKey) (*gatewayapiv1beta1.Gateway, error) {
	logger, _ := logr.FromContext(ctx)

	gw := &gatewayapiv1beta1.Gateway{}
	err := r.Client().Get(ctx, key, gw)
	logger.V(1).Info("FetchValidGateway", "gateway", key, "err", err)
	if err != nil {
		return nil, err
	}

	return gw, nil
}
