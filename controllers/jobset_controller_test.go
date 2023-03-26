package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	jobsetv1 "tutorial.kubebuilder.io/project/api/v1"
)

var _ = Describe("JobSet controller", func() {

	// Define utility constants for object names and testing timeouts/durations and intervals.
	const (
		JobsetName      = "test-jobset"
		JobsetNamespace = "default"
		JobName         = "test-job"

		timeout  = time.Second * 10
		duration = time.Second * 30
		interval = time.Millisecond * 250
	)

	Context("When updating JobSet Status", func() {
		It("Should increase JobSet Status.Active count when new Jobs are created", func() {
			By("By creating a new JobSet")
			indexedCompletionMode := batchv1.IndexedCompletion
			ctx := context.Background()
			jobSet := &jobsetv1.JobSet{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "batch.tutorial.kubebuilder.io/v1",
					Kind:       "JobSet",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      JobsetName,
					Namespace: JobsetNamespace,
				},
				Spec: jobsetv1.JobSetSpec{
					Jobs: []jobsetv1.JobTemplate{
						{
							Name:    "test-job-1-template-name",
							Network: &jobsetv1.Network{HeadlessService: pointer.Bool(true)},
							Template: &batchv1.JobTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Name:      "test-job-1-name",
									Namespace: JobsetNamespace,
								},
								Spec: batchv1.JobSpec{
									CompletionMode: &indexedCompletionMode,
									Parallelism:    pointer.Int32(1),
									Completions:    pointer.Int32(1),
									Template: corev1.PodTemplateSpec{
										Spec: corev1.PodSpec{
											RestartPolicy: "Never",
											Containers: []corev1.Container{
												{
													Name:  "test-container-1-name",
													Image: "test-container-1-image",
												},
											},
										},
									},
								},
							},
						},
						{
							Name:    "test-job-2-template-name",
							Network: &jobsetv1.Network{HeadlessService: pointer.Bool(true)},
							Template: &batchv1.JobTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Name:      "test-job-2-name",
									Namespace: JobsetNamespace,
								},
								Spec: batchv1.JobSpec{
									CompletionMode: &indexedCompletionMode,
									Parallelism:    pointer.Int32(4),
									Completions:    pointer.Int32(4),
									Template: corev1.PodTemplateSpec{
										Spec: corev1.PodSpec{
											RestartPolicy: "Never",
											Containers: []corev1.Container{
												{
													Name:  "test-container-2-name",
													Image: "test-container-2-image",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, jobSet)).Should(Succeed())

			jobSetLookupKey := types.NamespacedName{Name: JobsetName, Namespace: JobsetNamespace}
			createdJobSet := &jobsetv1.JobSet{}

			// We'll need to retry getting this newly created JobSet, given that creation may not immediately happen.
			Eventually(func() bool {
				err := k8sClient.Get(ctx, jobSetLookupKey, createdJobSet)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			By("Check JobSet eventually has 2 active jobs")
			Eventually(func() (int, error) {
				err := k8sClient.Get(ctx, jobSetLookupKey, createdJobSet)
				if err != nil {
					return -1, err
				}
				if len(createdJobSet.Status.Active) < 1 {
					return -1, nil
				}
				return len(createdJobSet.Status.Active), nil
			}, duration, interval).Should(Equal(2))
		})
	})
})
