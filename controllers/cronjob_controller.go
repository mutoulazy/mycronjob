/*
Copyright 2021.

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

package controllers

import (
	"context"
	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/reference"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	mybatchv1 "mycronjob/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
	Clock
}

var (
	jobOwnerKey             = ".metadata.controller"
	apiGVStr                = mybatchv1.GroupVersion.String()
	scheduledTimeAnnotation = "batch.tutorial.kubebuilder.io/scheduled-at"
)

type realClock struct{}

func (rc realClock) now() time.Time {
	return time.Now()
}

type Clock interface {
	now() time.Time
}

// +kubebuilder:docs-gen:collapse=Clock

//+kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("cronjob", req.NamespacedName)
	log.Info("start cronjob reconcile...")

	// 1. list cronjob by NamespacedName
	var cronJob mybatchv1.CronJob
	if err := r.Get(ctx, req.NamespacedName, &cronJob); err != nil {
		log.Error(err, "not found cronjob")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. list childJobs with cronjob
	var childJobs batchv1.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "not found childJobs")
		return ctrl.Result{}, err
	}

	// get childJobs status
	var activeJobs []*batchv1.Job
	var successfulJobs []*batchv1.Job
	var failedJobs []*batchv1.Job
	var mostRecentTime *time.Time

	// isJobFinished
	isJobFinished := func(job *batchv1.Job) (bool, batchv1.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == v1.ConditionTrue {
				return true, c.Type
			}
		}
		return false, ""
	}
	// +kubebuilder:docs-gen:collapse=isJobFinished

	// getScheduledTimeForJob
	getScheduledTimeForJob := func(job *batchv1.Job) (*time.Time, error) {
		timeRaw := job.Annotations[scheduledTimeAnnotation]
		if len(timeRaw) == 0 {
			return nil, nil
		}
		timeParsed, err := time.Parse(time.RFC3339, timeRaw)
		if err != nil {
			return nil, err
		}
		return &timeParsed, nil
	}
	// +kubebuilder:docs-gen:collapse=getScheduledTimeForJob

	// handle childJobs
	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": //ongoing
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case batchv1.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case batchv1.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}

		// get mostRecentTime
		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}
		if scheduledTimeForJob != nil {
			if mostRecentTime == nil {
				mostRecentTime = scheduledTimeForJob
			} else if mostRecentTime.Before(*scheduledTimeForJob) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	// handle cronjob status
	if mostRecentTime != nil {
		cronJob.Status.LastScheduleTime = &metav1.Time{*mostRecentTime}
	} else {
		cronJob.Status.LastScheduleTime = nil
	}
	cronJob.Status.Active = nil
	for _, activeJob := range activeJobs {
		jobRef, err := reference.GetReference(r.Scheme, activeJob)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	}

	log.V(1).Info("job count", "active jobs", len(activeJobs),
		"successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))

	if err := r.Status().Update(ctx, &cronJob); err != nil {
		log.Error(err, "unable to cronjob status")
		return ctrl.Result{}, err
	}

	// 3 Clean up old jobs according to the history limit

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mybatchv1.CronJob{}).
		Complete(r)
}
