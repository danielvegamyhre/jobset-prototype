package controllers

import (
	jobsetv1alpha "batch.x-k8s.io/jobset/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

func isJobFinished(job *batchv1.Job) (bool, batchv1.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

// TODO: is there a better way to check this?
func shouldSkipJob(jobName string, jobs *childJobs) bool {
	// skip if already active
	for _, job := range jobs.active {
		if job.Name == jobName {
			return true
		}
	}
	// skip if already succeeded
	for _, job := range jobs.successful {
		if job.Name == jobName {
			return true
		}
	}
	return false
}

func shouldStartJob(jobSet *jobsetv1alpha.JobSet, jobIdx int, jobs *childJobs) bool {
	if jobIdx == 0 {
		return true
	}
	prevJobIdx := jobIdx - 1
	for i, job := range jobs.active {
		// If prev job is ready or completed, this job can start.
		if job.Status.Ready != nil && job.Spec.Completions != nil {
			prevJobReady := *job.Status.Ready == *job.Spec.Completions
			prevJobSucceeded := job.Status.Succeeded == *job.Spec.Completions
			if i == prevJobIdx && (prevJobReady || prevJobSucceeded) {
				return true
			}
		}
	}
	klog.Infof("NOT STARTING JOB: %s", jobSet.Spec.Jobs[jobIdx].Name)
	return false
}
