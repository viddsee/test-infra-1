/*
Copyright 2017 The Kubernetes Authors.

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

// Package pjutil contains helpers for working with ProwJobs.
package pjutil

import (
	"path/filepath"
	"strconv"

	"github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"fmt"

	"k8s.io/api/core/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"

	"os/exec"
	"strings"

	buildv1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
)

const (
	jobNameLabel = "prow.k8s.io/job"
	jobTypeLabel = "prow.k8s.io/type"
	orgLabel     = "prow.k8s.io/refs.org"
	repoLabel    = "prow.k8s.io/refs.repo"
	pullLabel    = "prow.k8s.io/refs.pull"

	// todo copied from prow/pod-utils/downwardapi/jobspec.go - let's figure out a better way tp reuse these consts
	// JobSpecEnv is the name that contains JobSpec marshaled into a string.
	JobSpecEnv         = "JOB_SPEC"
	jobNameEnv         = "JOB_NAME"
	jobTypeEnv         = "JOB_TYPE"
	prowJobIDEnv       = "PROW_JOB_ID"
	buildIDEnv         = "BUILD_ID"
	jenkinsXBuildIDEnv = "JX_BUILD_NUMBER"
	repoOwnerEnv       = "REPO_OWNER"
	repoNameEnv        = "REPO_NAME"
	pullBaseRefEnv     = "PULL_BASE_REF"
	pullBaseShaEnv     = "PULL_BASE_SHA"
	pullRefsEnv        = "PULL_REFS"
	pullNumberEnv      = "PULL_NUMBER"
	pullPullShaEnv     = "PULL_PULL_SHA"
	cloneURI           = "CLONE_URI"
	// todo lets come up with better const names
	jmbrBranchName = "BRANCH_NAME"
	jmbrSourceURL  = "SOURCE_URL"
)

// NewProwJob initializes a ProwJob out of a ProwJobSpec with annotations.
func NewProwJobWithAnnotation(spec kube.ProwJobSpec, labels, annotations map[string]string) kube.ProwJob {
	return newProwJob(spec, labels, annotations)
}

// NewProwJob initializes a ProwJob out of a ProwJobSpec.
func NewProwJob(spec kube.ProwJobSpec, labels map[string]string) kube.ProwJob {
	return newProwJob(spec, labels, nil)
}

func newProwJob(spec kube.ProwJobSpec, labels, annotations map[string]string) kube.ProwJob {
	allLabels := map[string]string{
		jobNameLabel: spec.Job,
		jobTypeLabel: string(spec.Type),
	}
	if spec.Type != kube.PeriodicJob {
		allLabels[orgLabel] = spec.Refs.Org
		allLabels[repoLabel] = spec.Refs.Repo
		if len(spec.Refs.Pulls) > 0 {
			allLabels[pullLabel] = strconv.Itoa(spec.Refs.Pulls[0].Number)
		}
	}
	for key, value := range labels {
		allLabels[key] = value
	}

	// let's validate labels
	for key, value := range allLabels {
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			// try to use basename of a path, if path contains invalid //
			base := filepath.Base(value)
			if errs := validation.IsValidLabelValue(base); len(errs) == 0 {
				allLabels[key] = base
				continue
			}
			delete(allLabels, key)
			logrus.Warnf("Removing invalid label: key - %s, value - %s, error: %s", key, value, errs)
		}
	}

	return kube.ProwJob{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "prow.k8s.io/v1",
			Kind:       "ProwJob",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        uuid.NewV1().String(),
			Labels:      allLabels,
			Annotations: annotations,
		},
		Spec: spec,
		Status: kube.ProwJobStatus{
			StartTime: metav1.Now(),
			State:     kube.TriggeredState,
		},
	}
}

func NewPresubmit(pr github.PullRequest, baseSHA string, job config.Presubmit, eventGUID string) kube.ProwJob {
	org := pr.Base.Repo.Owner.Login
	repo := pr.Base.Repo.Name
	number := pr.Number
	kr := kube.Refs{
		Org:     org,
		Repo:    repo,
		BaseRef: pr.Base.Ref,
		BaseSHA: baseSHA,
		Pulls: []kube.Pull{
			{
				Number: number,
				Author: pr.User.Login,
				SHA:    pr.Head.SHA,
			},
		},
	}
	labels := make(map[string]string)
	for k, v := range job.Labels {
		labels[k] = v
	}
	labels[github.EventGUID] = eventGUID
	return NewProwJob(PresubmitSpec(job, kr), labels)
}

// PresubmitSpec initializes a ProwJobSpec for a given presubmit job.
func PresubmitSpec(p config.Presubmit, refs kube.Refs) kube.ProwJobSpec {
	refs.PathAlias = p.PathAlias
	refs.CloneURI = p.CloneURI
	pjs := kube.ProwJobSpec{
		Type:      kube.PresubmitJob,
		Job:       p.Name,
		Refs:      &refs,
		ExtraRefs: p.ExtraRefs,

		Report:         !p.SkipReport,
		Context:        p.Context,
		RerunCommand:   p.RerunCommand,
		MaxConcurrency: p.MaxConcurrency,

		DecorationConfig: p.DecorationConfig,
	}
	pjs.Agent = kube.ProwJobAgent(p.Agent)
	if pjs.Agent == kube.KubernetesAgent {
		pjs.PodSpec = p.Spec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
	}
	if pjs.Agent == kube.BuildAgent {
		pjs.BuildSpec = p.BuildSpec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
		interpolateEnvVars(&pjs, refs)
	}
	for _, nextP := range p.RunAfterSuccess {
		pjs.RunAfterSuccess = append(pjs.RunAfterSuccess, PresubmitSpec(nextP, refs))
	}
	return pjs
}

// PostsubmitSpec initializes a ProwJobSpec for a given postsubmit job.
func PostsubmitSpec(p config.Postsubmit, refs kube.Refs) kube.ProwJobSpec {
	refs.PathAlias = p.PathAlias
	refs.CloneURI = p.CloneURI
	pjs := kube.ProwJobSpec{
		Type:      kube.PostsubmitJob,
		Job:       p.Name,
		Refs:      &refs,
		ExtraRefs: p.ExtraRefs,

		MaxConcurrency: p.MaxConcurrency,

		DecorationConfig: p.DecorationConfig,
	}
	pjs.Agent = kube.ProwJobAgent(p.Agent)
	if pjs.Agent == kube.KubernetesAgent {
		pjs.PodSpec = p.Spec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
	}
	if pjs.Agent == kube.BuildAgent {
		pjs.BuildSpec = p.BuildSpec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
		interpolateEnvVars(&pjs, refs)
	}
	for _, nextP := range p.RunAfterSuccess {
		pjs.RunAfterSuccess = append(pjs.RunAfterSuccess, PostsubmitSpec(nextP, refs))
	}
	return pjs
}

func interpolateEnvVars(pjs *kube.ProwJobSpec, refs kube.Refs) {
	//todo lets clean this up
	sourceURL := fmt.Sprintf("https://github.com/%s/%s.git", refs.Org, refs.Repo)
	if refs.CloneURI != "" {
		sourceURL = refs.CloneURI
	}

	if pjs.BuildSpec.Source == nil {
		pjs.BuildSpec.Source = &buildv1alpha1.SourceSpec{}
	}
	pjs.BuildSpec.Source.Git = &buildv1alpha1.GitSourceSpec{
		Url: sourceURL,
	}
	// todo taken from downwardapi.JobSpec, lets clean up the duplication
	env := map[string]string{
		jobNameEnv: pjs.Job,
		// TODO: figure out how to reliably get this even after pod restarts, we want the number to increase so maybe we
		// TODO: need to think about using an external resource, kubernetes / git / some other to work out the next build #
		//jmbrBuildNumber: "987654321",
		buildIDEnv: "1",
		//prowJobIDEnv: spec.ProwJobID,
		jobTypeEnv: string(pjs.Type),
	}

	// todo lets get the proper branch name as this maybe a release branch
	branchName := "master"
	pullNumber := ""
	pullPullSha := ""
	if len(refs.Pulls) == 1 {
		pullNumber = strconv.Itoa(refs.Pulls[0].Number)
		branchName = "PR-" + pullNumber
		pullPullSha = refs.Pulls[0].SHA
	}
	// enrich with jenkins multi branch plugin env vars
	env[jmbrBranchName] = branchName
	env[jmbrSourceURL] = sourceURL
	env[repoOwnerEnv] = refs.Org
	env[repoNameEnv] = refs.Repo
	env[pullBaseRefEnv] = refs.BaseRef
	env[pullBaseShaEnv] = refs.BaseSHA
	env[pullRefsEnv] = refs.String()
	env[cloneURI] = refs.CloneURI
	env[pullNumberEnv] = pullNumber
	env[pullPullShaEnv] = pullPullSha

	if pullPullSha != "" {
		pjs.BuildSpec.Source.Git.Revision = pullPullSha
	} else {
		pjs.BuildSpec.Source.Git.Revision = refs.BaseSHA
	}
	env[jenkinsXBuildIDEnv] = getJenkinsXBuildNumber(refs.Org, refs.Repo, branchName)

	if nil != pjs.BuildSpec.Template {
		if len(pjs.BuildSpec.Template.Env) == 0 {
			pjs.BuildSpec.Template.Env = []v1.EnvVar{}
		}
		for k, v := range env {
			e := v1.EnvVar{
				Name:  k,
				Value: v,
			}
			pjs.BuildSpec.Template.Env = append(pjs.BuildSpec.Template.Env, e)
		}
	}

	for i, step := range pjs.BuildSpec.Steps {
		if len(step.Env) == 0 {
			step.Env = []v1.EnvVar{}
		}
		for k, v := range env {
			e := v1.EnvVar{
				Name:  k,
				Value: v,
			}
			pjs.BuildSpec.Steps[i].Env = append(pjs.BuildSpec.Steps[i].Env, e)
		}
	}
}
func getJenkinsXBuildNumber(org, repo, branch string) string {
	out, err := exec.Command("/jx", "step", "next-buildno", "-o", org, "-r", repo, "-b", strings.ToLower(branch)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// PeriodicSpec initializes a ProwJobSpec for a given periodic job.
func PeriodicSpec(p config.Periodic) kube.ProwJobSpec {
	pjs := kube.ProwJobSpec{
		Type:      kube.PeriodicJob,
		Job:       p.Name,
		ExtraRefs: p.ExtraRefs,

		DecorationConfig: p.DecorationConfig,
	}
	pjs.Agent = kube.ProwJobAgent(p.Agent)
	if pjs.Agent == kube.KubernetesAgent {
		pjs.PodSpec = p.Spec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
	}
	if pjs.Agent == kube.BuildAgent {
		pjs.BuildSpec = p.BuildSpec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
	}
	for _, nextP := range p.RunAfterSuccess {
		pjs.RunAfterSuccess = append(pjs.RunAfterSuccess, PeriodicSpec(nextP))
	}
	return pjs
}

// BatchSpec initializes a ProwJobSpec for a given batch job and ref spec.
func BatchSpec(p config.Presubmit, refs kube.Refs) kube.ProwJobSpec {
	refs.PathAlias = p.PathAlias
	refs.CloneURI = p.CloneURI
	pjs := kube.ProwJobSpec{
		Type:      kube.BatchJob,
		Job:       p.Name,
		Refs:      &refs,
		ExtraRefs: p.ExtraRefs,
		Context:   p.Context,

		DecorationConfig: p.DecorationConfig,
	}
	pjs.Agent = kube.ProwJobAgent(p.Agent)
	if pjs.Agent == kube.KubernetesAgent {
		pjs.PodSpec = p.Spec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
	}
	if pjs.Agent == kube.BuildAgent {
		pjs.BuildSpec = p.BuildSpec
		pjs.Cluster = p.Cluster
		if pjs.Cluster == "" {
			pjs.Cluster = kube.DefaultClusterAlias
		}
		interpolateEnvVars(&pjs, refs)
	}
	for _, nextP := range p.RunAfterSuccess {
		pjs.RunAfterSuccess = append(pjs.RunAfterSuccess, BatchSpec(nextP, refs))
	}
	return pjs
}

// PartitionActive separates the provided prowjobs into pending and triggered
// and returns them inside channels so that they can be consumed in parallel
// by different goroutines. Complete prowjobs are filtered out. Controller
// loops need to handle pending jobs first so they can conform to maximum
// concurrency requirements that different jobs may have.
func PartitionActive(pjs []kube.ProwJob) (pending, triggered chan kube.ProwJob) {
	// Size channels correctly.
	pendingCount, triggeredCount := 0, 0
	for _, pj := range pjs {
		switch pj.Status.State {
		case kube.PendingState:
			pendingCount++
		case kube.SuccessState:
			triggeredCount++
		case kube.FailureState:
			triggeredCount++
		case kube.TriggeredState:
			triggeredCount++
		}
	}
	pending = make(chan kube.ProwJob, pendingCount)

	triggered = make(chan kube.ProwJob, triggeredCount)
	// Partition the jobs into the two separate channels.
	for _, pj := range pjs {
		switch pj.Status.State {
		case kube.PendingState:
			pending <- pj
			// todo pretty sure this is bad and we should update success and failure elsewhere
		case kube.SuccessState:
			triggered <- pj
		case kube.FailureState:
			triggered <- pj
		case kube.TriggeredState:
			triggered <- pj
		}
	}
	close(pending)
	close(triggered)
	return pending, triggered
}

// GetLatestProwJobs filters through the provided prowjobs and returns
// a map of jobType jobs to their latest prowjobs.
func GetLatestProwJobs(pjs []kube.ProwJob, jobType kube.ProwJobType) map[string]kube.ProwJob {
	latestJobs := make(map[string]kube.ProwJob)
	for _, j := range pjs {
		if j.Spec.Type != jobType {
			continue
		}
		name := j.Spec.Job
		if j.Status.StartTime.After(latestJobs[name].Status.StartTime.Time) {
			latestJobs[name] = j
		}
	}
	return latestJobs
}

// ProwJobFields extracts logrus fields from a prowjob useful for logging.
func ProwJobFields(pj *kube.ProwJob) logrus.Fields {
	fields := make(logrus.Fields)
	fields["name"] = pj.ObjectMeta.Name
	fields["job"] = pj.Spec.Job
	fields["type"] = pj.Spec.Type
	if len(pj.ObjectMeta.Labels[github.EventGUID]) > 0 {
		fields[github.EventGUID] = pj.ObjectMeta.Labels[github.EventGUID]
	}
	if pj.Spec.Refs != nil && len(pj.Spec.Refs.Pulls) == 1 {
		fields[github.PrLogField] = pj.Spec.Refs.Pulls[0].Number
		fields[github.RepoLogField] = pj.Spec.Refs.Repo
		fields[github.OrgLogField] = pj.Spec.Refs.Org
	}
	return fields
}
