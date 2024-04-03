/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	batchv1 "github.com/1eedaegon/cronjob-operator/api/v1"
	kbatchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ref "k8s.io/client-go/tools/reference"

	"github.com/robfig/cron"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	scheduledTimeAnnotation = "batch.1eedaegon.github.io/scheduled-at"
	jobOwnerKey             = ".metadata.controller"
	apiGVStr                = batchv1.GroupVersion.String()
)

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

type Clock interface {
	Now() time.Time
}

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

//+kubebuilder:rbac:groups=batch.1eedaegon.github.io,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.1eedaegon.github.io,resources=cronjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.1eedaegon.github.io,resources=cronjobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.0/pkg/reconcile
/*
	1. CronJob yaml 로드
	2. Active 상태인 cronjob 목록을 가져오고 상태 업데이트
	3. history limit에 따라 오래된 job 정리
	4. Suspend하면 중지
	5. 다음 Cronjob 로드
	6. 과거 스케쥴이 아니면서 동시성 옵션에 블락걸리지 않은 CronJob 실행
	7. 실행중인 작업이 있거나 다음 예정 시간이 되면 대기열 삽입
*/
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// 1. CronJob yaml 로드
	var cronJob batchv1.CronJob
	if err := r.Get(ctx, req.NamespacedName, &cronJob); err != nil {
		log.Error(err, "unable to fetch CronJob")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, err
	}

	// 2. Active 상태인 cronjob 목록을 가져오고 상태 업데이트
	// jobOwnerKey를 이용해서 index를 memory에 cache
	var childJobs kbatchv1.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "unable to list child jobs")
		return ctrl.Result{}, err
	}

	var activeJobs []*kbatchv1.Job
	var successfulJobs []*kbatchv1.Job
	var failedJobs []*kbatchv1.Job
	var mostRecentTime *time.Time

	// job의 condition type이 completed거나 failed면서 coditionStatus true면 finished 처리
	isJobFinished := func(job *kbatchv1.Job) (bool, kbatchv1.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == kbatchv1.JobComplete || c.Type == kbatchv1.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}
		return false, ""
	}

	// getScheduledTimeForJob
	// 어노테이션에 scheduled-at이 붙어있으면 파싱, 없으면 nil
	getScheduledTimeForJob := func(job *kbatchv1.Job) (*time.Time, error) {
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

	// job의 상태를 판단해서 activeJob, failedJob, successfulJobs 어레이에 넣는다.
	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // ongoing
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case kbatchv1.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case kbatchv1.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}
		// 시작시간을 어노테이션에 담아야 한다. 따라서 active jobs이 스스로 담게한다.
		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}
		if scheduledTimeForJob != nil {
			// scheduledTimeForJob보다 이전이거나 recenttime이 없으면 mostRecentime에 scheduledTimeForJob을 할당한다.
			if mostRecentTime == nil || mostRecentTime.Before(*scheduledTimeForJob) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	if mostRecentTime != nil {
		cronJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		cronJob.Status.LastScheduleTime = nil
	}
	cronJob.Status.Active = nil

	for _, activeJob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	}
	log.V(1).Info("job count", "active job", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))

	if err := r.Status().Update(ctx, &cronJob); err != nil {
		log.Error(err, "unable to update CronJob status")
		return ctrl.Result{}, err
	}

	if cronJob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i] == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i, job := range successfulJobs {
			if int32(i) >= int32(len(successfulJobs))-*cronJob.Spec.SuccessfulJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old success job", "job", job)
			} else {
				log.V(0).Info("deleted old success job", "job", job)
			}
		}
	}

	// 3: Clean up old jobs according to the history limit
	// 3. history limit 이 초과한 히스토리 job 삭제
	if cronJob.Spec.FailedJobsHistoryLimit != nil {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i] == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*cronJob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old failed job", "job", job)
			} else {
				log.V(0).Info("deleted old failed job", "job", job)
			}
		}
	}

	// 4. cronjob의 spec이 suspend(중지중)이면 reconcile 스킵
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		log.V(1).Info("cronjob suspended, skipping")
		return ctrl.Result{}, nil
	}

	// 5. 다음 scheduled job을 실행
	getNextSchedule := func(cronJob *batchv1.CronJob, now time.Time) (lastMissed time.Time, next time.Time, err error) {
		sched, err := cron.ParseStandard(cronJob.Spec.Schedule)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("Unparseable schedule %q: %v", cronJob.Spec.Schedule, err)
		}
		var earliestTime time.Time
		if cronJob.Status.LastScheduleTime != nil {
			earliestTime = cronJob.Status.LastScheduleTime.Time
		} else {
			earliestTime = cronJob.ObjectMeta.CreationTimestamp.Time
		}
		if cronJob.Spec.StartingDeadlineSeconds != nil {
			schedulingDeadline := now.Add(-time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds))
			if schedulingDeadline.After(earliestTime) {
				earliestTime = schedulingDeadline
			}
		}
		if earliestTime.After(now) {
			return time.Time{}, sched.Next(now), nil
		}
		starts := 0
		for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
			lastMissed = t
			starts++
			if starts > 100 {
				return time.Time{},
					time.Time{},
					fmt.Errorf("Too many missed start times (> 100). set or decreased .spec.startingDeadlineSeconds or check clock skew.")
			}
		}
		return lastMissed, sched.Next(now), nil
	}

	missedRun, nextRun, err := getNextSchedule(&cronJob, r.Now())
	if err != nil {
		log.Error(err, "unable to figure out CronJob schedule")
		return ctrl.Result{}, nil
	}

	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())}
	log = log.WithValues("now", r.Now(), "next run", nextRun)

	constructJobForCronJob := func(cronJob *batchv1.CronJob, scheduleTime time.Time) (*kbatchv1.Job, error) {
		name := fmt.Sprintf("%s-%d", cronJob.Name, scheduleTime.Unix())
		job := &kbatchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
				Name:        name,
				Namespace:   cronJob.Namespace,
			},
			Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
		}
		for k, v := range cronJob.Spec.JobTemplate.Annotations {
			job.Annotations[k] = v
		}
		job.Annotations[scheduledTimeAnnotation] = scheduleTime.Format(time.RFC3339)
		for k, v := range cronJob.Spec.JobTemplate.Labels {
			job.Labels[k] = v
		}
		if err := ctrl.SetControllerReference(cronJob, job, r.Scheme); err != nil {
			return nil, err
		}
		return job, nil
	}

	job, err := constructJobForCronJob(&cronJob, missedRun)
	if err != nil {
		log.Error(err, "unable to construct job from template")
		return scheduledResult, nil
	}

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "unable to create Job for CronJob", "job", job)
		return ctrl.Result{}, err
	}
	log.V(1).Info("created Job for CronJob run", "job", job)
	return scheduledResult, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kbatchv1.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		job := rawObj.(*kbatchv1.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		if owner.APIVersion != apiGVStr || owner.Kind != "CronJob" {
			return nil
		}
		return []string{owner.Name}
	}); err != nil {
		return nil
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.CronJob{}).
		Owns(&kbatchv1.Job{}).
		Complete(r)
}
