package conditions_test

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/Kuadrant/multicluster-gateway-controller/pkg/_internal/conditions"
	"github.com/Kuadrant/multicluster-gateway-controller/pkg/apis/v1alpha1"
)

const (
	testConditionType conditions.ConditionType = "testCondition"
)

func TestBuildPolicyCondition(t *testing.T) {
	runtimeObject := func() runtime.Object {
		return &v1alpha1.DNSPolicy{
			TypeMeta: metav1.TypeMeta{
				Kind:       "DNSPolicy",
				APIVersion: "kuadrant.io/v1alpha1",
			},
		}
	}

	targetRef := func() *gatewayv1beta1.Gateway {
		return &gatewayv1beta1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Generation: int64(2),
			},
		}
	}
	testCases := []struct {
		Name            string
		ConditionReason conditions.ConditionReason
		Err             error
		Validate        func(t *testing.T, cond metav1.Condition)
	}{
		{
			Name:            "test condition accepted",
			ConditionReason: conditions.PolicyReasonAccepted,
			Validate: func(t *testing.T, cond metav1.Condition) {
				if cond.Reason != string(conditions.PolicyReasonAccepted) {
					t.Fatalf("expected condition reason %s but got %s ", conditions.PolicyReasonAccepted, cond.Reason)
				}
				if cond.ObservedGeneration != targetRef().Generation {
					t.Fatalf("expected observed generation %d but got %d", targetRef().Generation, cond.ObservedGeneration)
				}
			},
		},
		{
			Name:            "test condition invalid",
			ConditionReason: conditions.PolicyReasonInvalid,
			Validate: func(t *testing.T, cond metav1.Condition) {
				if cond.Reason != string(conditions.PolicyReasonInvalid) {
					t.Fatalf("expected condition reason %s but got %s ", conditions.PolicyReasonAccepted, cond.Reason)
				}
				if cond.ObservedGeneration != targetRef().Generation {
					t.Fatalf("expected observed generation %d but got %d", targetRef().Generation, cond.ObservedGeneration)
				}
			},
			Err: fmt.Errorf("fatal error"),
		},
		{
			Name:            "test condition conflicted",
			ConditionReason: conditions.PolicyReasonConflicted,
			Validate: func(t *testing.T, cond metav1.Condition) {
				if cond.Reason != string(conditions.PolicyReasonConflicted) {
					t.Fatalf("expected condition reason %s but got %s ", conditions.PolicyReasonConflicted, cond.Reason)
				}
				if cond.ObservedGeneration != targetRef().Generation {
					t.Fatalf("expected observed generation %d but got %d", targetRef().Generation, cond.ObservedGeneration)
				}
			},
			Err: fmt.Errorf("fatal error"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			cond := conditions.BuildPolicyAffectedCondition(testConditionType, runtimeObject(), targetRef(), testCase.ConditionReason, testCase.Err)
			testCase.Validate(t, cond)
		})
	}

}
