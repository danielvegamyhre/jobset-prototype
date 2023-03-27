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

package controllers

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	jobsetv1 "batch.x-k8s.io/jobset/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclientset "k8s.io/client-go/kubernetes"
)

// JobSetReconciler reconciles a JobSet object
type JobSetReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	KubeClientSet *kubeclientset.Clientset
	Clock
}

type childJobs struct {
	active     []*batchv1.Job
	successful []*batchv1.Job
	failed     []*batchv1.Job
}

var (
	jobOwnerKey = ".metadata.controller"
	apiGVStr    = jobsetv1.GroupVersion.String()
)

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

// clock knows how to get the current time.
// It can be used to fake out timing for testing.
type Clock interface {
	Now() time.Time
}

//+kubebuilder:rbac:groups=batch.x-k8s.io,resources=jobsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.x-k8s.io,resources=jobsets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.x-k8s.io,resources=jobsets/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the JobSet object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *JobSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var jobSet jobsetv1.JobSet
	if err := r.Get(ctx, req.NamespacedName, &jobSet); err != nil {
		log.Error(err, "unable to fetch JobSet")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	jobs, err := r.getChildJobs(ctx, &jobSet, req, log)
	if err != nil {
		return ctrl.Result{}, nil
	}

	if err := r.updateStatus(ctx, &jobSet, jobs, log); err != nil {
		return ctrl.Result{}, nil
	}

	r.cleanUpOldJobs(ctx, jobs, log)

	if err := r.createNewJobs(ctx, req, &jobSet, jobs, log); err != nil {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JobSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// set up a real clock, since we're not in a test
	if r.Clock == nil {
		r.Clock = realClock{}
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &batchv1.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*batchv1.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...make sure it's a JobSet...
		if owner.APIVersion != apiGVStr || owner.Kind != "JobSet" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&jobsetv1.JobSet{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func (r *JobSetReconciler) constructJobFromTemplate(jobSet *jobsetv1.JobSet, jobTemplate *jobsetv1.JobTemplate) (*batchv1.Job, error) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        jobTemplate.Name,
			Namespace:   jobSet.Namespace,
		},
		Spec: *jobTemplate.Template.Spec.DeepCopy(),
	}
	// Set controller owner reference for garbage collection and reconcilation.
	if err := ctrl.SetControllerReference(jobSet, job, r.Scheme); err != nil {
		return nil, err
	}
	return job, nil
}

// cleanUpOldJobs does "best effort" deletion of old jobs - if we fail on
// a particular one, we won't requeue just to finish the deleting.
func (r *JobSetReconciler) cleanUpOldJobs(ctx context.Context, jobs *childJobs, log logr.Logger) {
	// Clean up failed jobs
	for _, job := range jobs.failed {
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to delete old failed job", "job", job)
		} else {
			log.V(0).Info("deleted old failed job", "job", job)
		}
	}

	// Clean up succeeded jobs
	for _, job := range jobs.successful {
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); (err) != nil {
			log.Error(err, "unable to delete old successful job", "job", job)
		} else {
			log.V(0).Info("deleted old successful job", "job", job)
		}
	}
}

// getChildJobs fetches all Jobs owned by the JobSet and returns them
// categorized by status (active, successful, failed).
func (r *JobSetReconciler) getChildJobs(ctx context.Context, jobSet *jobsetv1.JobSet, req ctrl.Request, log logr.Logger) (*childJobs, error) {
	// Get all active jobs owned by JobSet.
	var childJobList batchv1.JobList
	if err := r.List(ctx, &childJobList, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return nil, err
	}

	jobs := childJobs{}
	for i, job := range childJobList.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // active
			jobs.active = append(jobs.active, &childJobList.Items[i])
		case batchv1.JobFailed:
			jobs.failed = append(jobs.failed, &childJobList.Items[i])
		case batchv1.JobComplete:
			jobs.successful = append(jobs.successful, &childJobList.Items[i])
		}
	}

	return &jobs, nil
}

// updateStatus
func (r *JobSetReconciler) updateStatus(ctx context.Context, jobSet *jobsetv1.JobSet, jobs *childJobs, log logr.Logger) error {
	// TODO: Why is .Status.Active type []*corev1.ObjectReference instead of []*batchv1.Job?
	// Is it because this is a generic way for kubebuilder to generate boilerplate data structures for CRDs?
	jobSet.Status.Active = nil
	for _, activeJob := range jobs.active {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		jobSet.Status.Active = append(jobSet.Status.Active, *jobRef)
	}

	if err := r.Status().Update(ctx, jobSet); err != nil {
		log.Error(err, "unable to update JobSet status")
		return err
	}
	log.V(1).Info("job count", "active jobs", len(jobs.active), "successful jobs", len(jobs.successful), "failed jobs", len(jobs.failed))
	return nil
}

func (r *JobSetReconciler) createNewJobs(ctx context.Context, req ctrl.Request, jobSet *jobsetv1.JobSet, jobs *childJobs, log logr.Logger) error {
	// Create jobs as necessary.
	for i, jobTemplate := range jobSet.Spec.Jobs {
		job, err := r.constructJobFromTemplate(jobSet, &jobTemplate)
		if err != nil {
			log.Error(err, "unable to construct job from template", "jobTemplate", jobTemplate)
			return err
		}
		// Skip this job if it is already active.
		if isJobActive(jobs.active, job.Name) {
			log.Info("job is already active", "job", job)
			continue
		}

		// Only create job if previous job is ready and this job is not yet active.
		if i == 0 || isPrevJobReady(jobs.active, jobSet.Spec.Jobs[i-1].Name) {
			// First create headless service if specified for this job.
			if jobTemplate.Network.HeadlessService != nil && *jobTemplate.Network.HeadlessService {
				if err := r.createHeadlessSvcIfNotExist(ctx, req, jobSet, job, log); err != nil {
					return err
				}
				// Update job spec to set subdomain as headless service name (will always be same as job name)
				job.Spec.Template.Spec.Subdomain = job.Name
			}

			if err := r.Create(ctx, job); err != nil {
				log.Error(err, "unable to create Job for JobSet", "job", job)
				return err
			}
			log.V(1).Info("created Job for JobSet run", "job", job)
		}
	}
	return nil
}

func (r *JobSetReconciler) createHeadlessSvcIfNotExist(ctx context.Context, req ctrl.Request, jobSet *jobsetv1.JobSet, job *batchv1.Job, log logr.Logger) error {
	// Check if service already exists. Service name is same as job name.
	if _, err := r.KubeClientSet.CoreV1().Services(req.Namespace).Get(ctx, job.Name, metav1.GetOptions{}); err != nil {
		headlessSvc := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      job.Name,
				Namespace: req.Namespace,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "None",
				Ports: []corev1.ServicePort{
					{
						Port: 8443,
					},
				},
				Selector: map[string]string{
					"job-name": job.Name,
				},
			},
		}
		// set controller owner reference for garbage collection and reconcilation
		if err := ctrl.SetControllerReference(jobSet, &headlessSvc, r.Scheme); err != nil {
			log.Error(err, "error setting controller owner reference for headless service", "service", headlessSvc)
			return err
		}
		if _, err := r.KubeClientSet.CoreV1().Services(req.Namespace).Create(ctx, &headlessSvc, metav1.CreateOptions{}); err != nil {
			log.Error(err, "unable to create headless service", "service", headlessSvc)
			return err
		}
	}
	return nil
}

func isJobFinished(job *batchv1.Job) (bool, batchv1.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

// TODO: is there a better way to check this?
func isJobActive(activeJobs []*batchv1.Job, jobName string) bool {
	for _, job := range activeJobs {
		if job.Name == jobName {
			return true
		}
	}
	return false
}

func isPrevJobReady(activeJobs []*batchv1.Job, prevJobName string) bool {
	for _, job := range activeJobs {
		if job.Name == prevJobName && job.Status.Ready == job.Spec.Parallelism {
			return true
		}
	}
	return false
}
