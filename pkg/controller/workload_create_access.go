// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadCreateAccessReviewer answers whether the controller may create a
// resource in a namespace. The Job controller asks it after a Forbidden
// workload create, to tell an RBAC denial (a missing RoleBinding, which the
// Workflow waits out under ADR-092) from an admission or quota rejection
// (which runs against the workload creation deadline, issue #488).
//
// envtest does not model a missing RoleBinding, so tests inject an
// implementation in place of the SelfSubjectAccessReview one.
type WorkloadCreateAccessReviewer interface {
	// CanCreate reports whether the controller may create resource in
	// namespace. An error means the answer is unknown.
	CanCreate(ctx context.Context, namespace string, resource schema.GroupResource) (bool, error)
}

// ssarCreateReviewer answers CanCreate with one SelfSubjectAccessReview. A
// review is a create that is never stored, so it always reaches the API
// server. It is only asked after a Forbidden create, which controller-runtime
// already backs off, so answers are not cached.
type ssarCreateReviewer struct{ client client.Client }

func (r ssarCreateReviewer) CanCreate(ctx context.Context, namespace string, resource schema.GroupResource) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "create",
				Group:     resource.Group,
				Resource:  resource.Resource,
			},
		},
	}
	if err := r.client.Create(ctx, review); err != nil {
		return false, fmt.Errorf("SelfSubjectAccessReview for create %s in namespace %q: %w", resource, namespace, err)
	}
	return review.Status.Allowed && !review.Status.Denied, nil
}

// forbiddenGroupResource returns the group and resource a Forbidden API error
// names. The API server fills StatusDetails with the resource it refused, so
// the review asks about exactly what was denied.
func forbiddenGroupResource(err error) (schema.GroupResource, bool) {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return schema.GroupResource{}, false
	}
	details := status.Status().Details
	if details == nil || details.Kind == "" {
		return schema.GroupResource{}, false
	}
	return schema.GroupResource{Group: details.Group, Resource: details.Kind}, true
}
