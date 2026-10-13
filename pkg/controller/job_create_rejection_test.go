// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

var trainJobsResource = schema.GroupResource{Group: "trainer.kubeflow.org", Resource: "trainjobs"}

// TestIsWorkloadCreateRejection pins which create errors run against the
// workload creation deadline (issue #488): API server rejections do, while
// errors that say nothing about the object keep retrying unbounded.
func TestIsWorkloadCreateRejection(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"webhook denial", apierrors.NewForbidden(trainJobsResource, "tj",
			errors.New(`admission webhook "validate.trainer" denied the request`)), true},
		{"quota exceeded", apierrors.NewForbidden(trainJobsResource, "tj",
			errors.New("exceeded quota: gpu-quota")), true},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: trainJobsResource.Group, Kind: testKindTrainJob},
			"tj", field.ErrorList{field.Required(field.NewPath("spec"), "")}), true},
		{"bad request", apierrors.NewBadRequest("bad"), true},
		{"server timeout", apierrors.NewServerTimeout(trainJobsResource, "create", 1), false},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"internal error", apierrors.NewInternalError(errors.New("etcd")), false},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isWorkloadCreateRejection(tt.err))
		})
	}
}

// TestCreateWorkloadFromSpecRejection covers the create-error paths of
// createWorkloadFromSpec with no timeoutPerJob in play: a rejection within
// the deadline keeps the Job InProgress and retrying, one past it fails the
// Job, a transient error never starts the clock, and a create that
// succeeds after rejections clears it.
func TestCreateWorkloadFromSpecRejection(t *testing.T) {
	forbidden := apierrors.NewForbidden(trainJobsResource, testJobName,
		errors.New("denied by ValidatingAdmissionPolicy"))
	transient := apierrors.NewServerTimeout(trainJobsResource, "create", 1)

	tests := []struct {
		name string
		// createErr is returned by every TrainJob create; nil lets it through.
		createErr error
		// rejectedFor, when non-zero, seeds WorkloadCreationRejectedSince
		// that long ago.
		rejectedFor   time.Duration
		wantErr       bool
		wantCondition string
		wantReason    string
		wantSinceSet  bool
	}{
		{
			name: "first rejection starts clock", createErr: forbidden, wantErr: true,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreationRejected,
			wantSinceSet: true,
		},
		{
			name: "rejection within deadline keeps retrying", createErr: forbidden,
			rejectedFor: 5 * time.Minute, wantErr: true,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreationRejected,
			wantSinceSet: true,
		},
		{
			name: "rejection past deadline fails job", createErr: forbidden,
			rejectedFor: 11 * time.Minute, wantErr: false,
			wantCondition: nvcrev1alpha1.JobFailed, wantReason: ReasonWorkloadCreationError,
			wantSinceSet: true,
		},
		{
			name: "transient error does not start clock", createErr: transient, wantErr: true,
		},
		{
			name: "transient error past deadline still retries", createErr: transient,
			rejectedFor: time.Hour, wantErr: true, wantSinceSet: true,
		},
		{
			name: "create after rejection clears clock", rejectedFor: 5 * time.Minute,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupJobMetrics(testNS, testJobName)
			ctx := context.Background()
			scheme := newJobLookupWorkloadScheme(t)

			job := newLookupJob(false)
			if tt.rejectedFor > 0 {
				since := metav1.NewTime(time.Now().Add(-tt.rejectedFor))
				job.Status.WorkloadCreationRejectedSince = &since
			}
			createErr := tt.createErr
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(job.DeepCopy()).
				WithStatusSubresource(&nvcrev1alpha1.Job{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, inner client.WithWatch, obj client.Object,
						opts ...client.CreateOption,
					) error {
						if _, isJob := obj.(*nvcrev1alpha1.Job); !isJob && createErr != nil {
							return createErr
						}
						return inner.Create(ctx, obj, opts...)
					},
				}).Build()
			key := client.ObjectKey{Namespace: testNS, Name: testJobName}
			require.NoError(t, c.Get(ctx, key, job))

			r := &JobReconciler{
				Client: c, Scheme: scheme, Recorder: events.NewFakeRecorder(20),
				WorkloadCreationDeadline: 10 * time.Minute,
			}
			_, err := r.createWorkloadFromSpec(ctx, job)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			got := &nvcrev1alpha1.Job{}
			require.NoError(t, c.Get(ctx, key, got))
			require.Equal(t, tt.wantSinceSet, got.Status.WorkloadCreationRejectedSince != nil)
			if tt.wantCondition == "" {
				require.Empty(t, got.Status.Conditions)
				return
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, tt.wantCondition)
			require.NotNil(t, cond)
			require.Equal(t, metav1.ConditionTrue, cond.Status)
			require.Equal(t, tt.wantReason, cond.Reason)
		})
	}
}

// TestSchedulingBlockedMessageCreationRejected checks that a Job whose
// workload create is being rejected is relayed to the Workflow.
func TestSchedulingBlockedMessageCreationRejected(t *testing.T) {
	job := &nvcrev1alpha1.Job{}
	job.Name = "j1"
	job.Status.Conditions = []metav1.Condition{{
		Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue,
		Reason: ReasonWorkloadCreationRejected, Message: "denied",
	}}
	require.Equal(t, "job j1 creation rejected: denied", schedulingBlockedMessage(job))
}
