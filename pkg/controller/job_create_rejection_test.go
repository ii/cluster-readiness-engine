// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
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

// Events as drainEventTypeReasons reports them.
const (
	evWarnCreationError    = "Warning " + ReasonWorkloadCreationError
	evWarnCreationRejected = "Warning " + ReasonWorkloadCreationRejected
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

// fakeCreateReviewer answers CanCreate with a fixed verdict and records how
// often it was asked.
type fakeCreateReviewer struct {
	allowed bool
	err     error
	asked   int
}

func (f *fakeCreateReviewer) CanCreate(context.Context, string, schema.GroupResource) (bool, error) {
	f.asked++
	return f.allowed, f.err
}

// TestForbiddenGroupResource checks the review asks about the resource the
// API server refused.
func TestForbiddenGroupResource(t *testing.T) {
	gr, ok := forbiddenGroupResource(apierrors.NewForbidden(trainJobsResource, "tj", errors.New("no")))
	require.True(t, ok)
	require.Equal(t, trainJobsResource, gr)
	_, ok = forbiddenGroupResource(errors.New("plain"))
	require.False(t, ok)
}

// TestCreateWorkloadFromSpecRejection covers the create-error paths of
// createWorkloadFromSpec with no timeoutPerJob in play: a rejection within
// the deadline keeps the Job InProgress and retrying, one past it fails the
// Job without recording failed nodes, an RBAC denial or an unconfirmed
// Forbidden never starts the clock, a transient error never starts it, and a
// create that succeeds after rejections clears it.
func TestCreateWorkloadFromSpecRejection(t *testing.T) {
	forbidden := apierrors.NewForbidden(trainJobsResource, testJobName,
		errors.New("denied by ValidatingAdmissionPolicy"))
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: trainJobsResource.Group, Kind: testKindTrainJob},
		testJobName, field.ErrorList{field.Required(field.NewPath("spec"), "")})
	transient := apierrors.NewServerTimeout(trainJobsResource, "create", 1)

	tests := []struct {
		name string
		// createErr is returned by every TrainJob create; nil lets it through.
		createErr error
		// rbacDenied makes the access review deny create; reviewErr makes it fail.
		rbacDenied bool
		reviewErr  error
		// rejectedFor, when non-zero, seeds WorkloadCreationRejectedSince
		// that long ago. alreadyRejected also seeds the InProgress condition.
		rejectedFor     time.Duration
		alreadyRejected bool
		wantErr         bool
		wantCondition   string
		wantReason      string
		wantSinceSet    bool
		wantReviewAsked bool
		// wantEvents lists "Type Reason" of every recorded Event.
		wantEvents []string
	}{
		{
			name: "first rejection starts clock", createErr: forbidden, wantErr: true,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreationRejected,
			wantSinceSet: true, wantReviewAsked: true,
			wantEvents: []string{evWarnCreationRejected},
		},
		{
			name: "invalid object is a rejection without an access review", createErr: invalid, wantErr: true,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreationRejected,
			wantSinceSet: true, wantEvents: []string{evWarnCreationRejected},
		},
		{
			name: "repeated rejection within deadline emits no event", createErr: forbidden,
			rejectedFor: 5 * time.Minute, alreadyRejected: true, wantErr: true,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreationRejected,
			wantSinceSet: true, wantReviewAsked: true,
		},
		{
			name: "rejection past deadline fails job without failed nodes", createErr: forbidden,
			rejectedFor: 11 * time.Minute, alreadyRejected: true, wantErr: false,
			wantCondition: nvcrev1alpha1.JobFailed, wantReason: ReasonWorkloadCreationError,
			wantSinceSet: true, wantReviewAsked: true,
			wantEvents: []string{evWarnCreationError},
		},
		{
			name: "rbac denial never starts clock", createErr: forbidden, rbacDenied: true,
			wantErr: true, wantReviewAsked: true,
			wantEvents: []string{evWarnCreationError},
		},
		{
			name: "rbac denial past deadline still retries", createErr: forbidden, rbacDenied: true,
			rejectedFor: time.Hour, wantErr: true, wantSinceSet: true, wantReviewAsked: true,
			wantEvents: []string{evWarnCreationError},
		},
		{
			name: "failed access review never fails job", createErr: forbidden,
			reviewErr: errors.New("review unavailable"), rejectedFor: time.Hour,
			wantErr: true, wantSinceSet: true, wantReviewAsked: true,
			wantEvents: []string{evWarnCreationError},
		},
		{
			name: "transient error does not start clock", createErr: transient, wantErr: true,
			wantEvents: []string{evWarnCreationError},
		},
		{
			name: "transient error past deadline still retries", createErr: transient,
			rejectedFor: time.Hour, wantErr: true, wantSinceSet: true,
			wantEvents: []string{evWarnCreationError},
		},
		{
			name: "create after rejection clears clock", rejectedFor: 5 * time.Minute,
			wantCondition: nvcrev1alpha1.JobInProgress, wantReason: ReasonWorkloadCreated,
			wantEvents: []string{"Normal WorkloadCreated"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupJobMetrics(testNS, testJobName)
			ctx := context.Background()
			scheme := newJobLookupWorkloadScheme(t)

			job := newLookupJob(false)
			// The group's nodes are known: a rejection must still not
			// attribute the failure to them.
			job.Annotations = map[string]string{"nvcre.nvidia.com/group-nodes": "gpu-node-01,gpu-node-02"}
			if tt.rejectedFor > 0 {
				since := metav1.NewTime(time.Now().Add(-tt.rejectedFor))
				job.Status.WorkloadCreationRejectedSince = &since
			}
			if tt.alreadyRejected {
				job.Status.Conditions = []metav1.Condition{{
					Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue,
					Reason: ReasonWorkloadCreationRejected, Message: "Workload TrainJob/" + testJobName + "-workload: " + tt.createErr.Error(),
					ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
				}}
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

			reviewer := &fakeCreateReviewer{allowed: !tt.rbacDenied, err: tt.reviewErr}
			recorder := events.NewFakeRecorder(20)
			r := &JobReconciler{
				Client: c, Scheme: scheme, Recorder: recorder,
				WorkloadCreationDeadline: 10 * time.Minute,
				CreateAccessReviewer:     reviewer,
			}
			_, err := r.createWorkloadFromSpec(ctx, job)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.wantReviewAsked, reviewer.asked > 0)

			got := &nvcrev1alpha1.Job{}
			require.NoError(t, c.Get(ctx, key, got))
			require.Equal(t, tt.wantSinceSet, got.Status.WorkloadCreationRejectedSince != nil)
			require.Empty(t, got.Status.FailedNodes, "a create rejection must not record failed nodes")
			require.Equal(t, tt.wantEvents, drainEventTypeReasons(recorder))
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

// drainEventTypeReasons returns "Type Reason" for every Event recorded so far.
func drainEventTypeReasons(r *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-r.Events:
			f := strings.Fields(e)
			if len(f) >= 2 {
				out = append(out, f[0]+" "+f[1])
			}
		default:
			return out
		}
	}
}

// TestSchedulingBlockedMessageCreationRejected checks that a Job whose
// workload create is being rejected is relayed to the Workflow, and that the
// relayed message says "rejected" once.
func TestSchedulingBlockedMessageCreationRejected(t *testing.T) {
	job := &nvcrev1alpha1.Job{}
	job.Name = "j1"
	job.Status.Conditions = []metav1.Condition{{
		Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue,
		Reason: ReasonWorkloadCreationRejected, Message: "Workload TrainJob/j1-workload: denied",
	}}
	require.Equal(t, "job j1 creation rejected: Workload TrainJob/j1-workload: denied", schedulingBlockedMessage(job))
}
