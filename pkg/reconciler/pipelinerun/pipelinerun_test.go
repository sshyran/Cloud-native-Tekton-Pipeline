/*
Copyright 2019 The Tekton Authors

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

package pipelinerun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/tektoncd/pipeline/pkg/apis/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/pod"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	resourcev1alpha1 "github.com/tektoncd/pipeline/pkg/apis/resource/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/reconciler/events/cloudevent"
	"github.com/tektoncd/pipeline/pkg/reconciler/pipelinerun/resources"
	ttesting "github.com/tektoncd/pipeline/pkg/reconciler/testing"
	"github.com/tektoncd/pipeline/pkg/reconciler/volumeclaim"
	resolutioncommon "github.com/tektoncd/pipeline/pkg/resolution/common"
	"github.com/tektoncd/pipeline/test"
	"github.com/tektoncd/pipeline/test/diff"
	eventstest "github.com/tektoncd/pipeline/test/events"
	"github.com/tektoncd/pipeline/test/names"
	"github.com/tektoncd/pipeline/test/parse"
	"gomodules.xyz/jsonpatch/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	duckv1beta1 "knative.dev/pkg/apis/duck/v1beta1"
	cminformer "knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	logtesting "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"

	_ "knative.dev/pkg/system/testing" // Setup system.Namespace()
)

var (
	images = pipeline.Images{
		EntrypointImage:          "override-with-entrypoint:latest",
		NopImage:                 "override-with-nop:latest",
		GitImage:                 "override-with-git:latest",
		KubeconfigWriterImage:    "override-with-kubeconfig-writer:latest",
		ShellImage:               "busybox",
		GsutilImage:              "gcr.io/google.com/cloudsdktool/cloud-sdk",
		PRImage:                  "override-with-pr:latest",
		ImageDigestExporterImage: "override-with-imagedigest-exporter-image:latest",
	}

	ignoreResourceVersion    = cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion")
	ignoreTypeMeta           = cmpopts.IgnoreFields(metav1.TypeMeta{}, "Kind", "APIVersion")
	ignoreLastTransitionTime = cmpopts.IgnoreFields(apis.Condition{}, "LastTransitionTime.Inner.Time")
	ignoreStartTime          = cmpopts.IgnoreFields(v1beta1.PipelineRunStatusFields{}, "StartTime")
	ignoreCompletionTime     = cmpopts.IgnoreFields(v1beta1.PipelineRunStatusFields{}, "CompletionTime")
	ignoreFinallyStartTime   = cmpopts.IgnoreFields(v1beta1.PipelineRunStatusFields{}, "FinallyStartTime")
	trueb                    = true
	simpleHelloWorldTask     = &v1beta1.Task{ObjectMeta: baseObjectMeta("hello-world", "foo")}
	simpleSomeTask           = &v1beta1.Task{ObjectMeta: baseObjectMeta("some-task", "foo")}
	simpleHelloWorldPipeline = &v1beta1.Pipeline{
		ObjectMeta: baseObjectMeta("test-pipeline", "foo"),
		Spec: v1beta1.PipelineSpec{
			Tasks: []v1beta1.PipelineTask{{
				Name: "hello-world-1",
				TaskRef: &v1beta1.TaskRef{
					Name: "hello-world",
				},
			}},
		},
	}

	now       = time.Date(2022, time.January, 1, 0, 0, 0, 0, time.UTC)
	testClock = clock.NewFakePassiveClock(now)
)

const (
	apiFieldsFeatureFlag           = "enable-api-fields"
	customTasksFeatureFlag         = "enable-custom-tasks"
	ociBundlesFeatureFlag          = "enable-tekton-oci-bundles"
	embeddedStatusFeatureFlag      = "embedded-status"
	maxMatrixCombinationsCountFlag = "default-max-matrix-combinations-count"
)

type PipelineRunTest struct {
	test.Data  `json:"inline"`
	Test       *testing.T
	TestAssets test.Assets
	Cancel     func()
}

// getPipelineRunController returns an instance of the PipelineRun controller/reconciler that has been seeded with
// d, where d represents the state of the system (existing resources) needed for the test.
func getPipelineRunController(t *testing.T, d test.Data) (test.Assets, func()) {
	return initializePipelineRunControllerAssets(t, d, pipeline.Options{Images: images})
}

// initiailizePipelinerunControllerAssets is a shared helper for
// controller initialization.
func initializePipelineRunControllerAssets(t *testing.T, d test.Data, opts pipeline.Options) (test.Assets, func()) {
	ctx, _ := ttesting.SetupFakeContext(t)
	ctx, cancel := context.WithCancel(ctx)
	test.EnsureConfigurationConfigMapsExist(&d)
	c, informers := test.SeedTestData(t, ctx, d)
	configMapWatcher := cminformer.NewInformedWatcher(c.Kube, system.Namespace())
	ctl := NewController(&opts, testClock)(ctx, configMapWatcher)
	if la, ok := ctl.Reconciler.(reconciler.LeaderAware); ok {
		if err := la.Promote(reconciler.UniversalBucket(), func(reconciler.Bucket, types.NamespacedName) {}); err != nil {
			t.Fatalf("error promoting reconciler leader: %v", err)
		}
	}
	if err := configMapWatcher.Start(ctx.Done()); err != nil {
		t.Fatalf("error starting configmap watcher: %v", err)
	}
	return test.Assets{
		Logger:     logging.FromContext(ctx),
		Clients:    c,
		Controller: ctl,
		Informers:  informers,
		Recorder:   controller.GetEventRecorder(ctx).(*record.FakeRecorder),
		Ctx:        ctx,
	}, cancel
}

// getTaskRunCreations will look through a set of actions to find all task run creation actions and return the set of
// them. It will fatal the test if none are found, or if less actions were found than expected.
func getTaskRunCreations(t *testing.T, actions []ktesting.Action, minActionCount int) []*v1beta1.TaskRun {
	t.Helper()
	if len(actions) < minActionCount {
		t.Fatalf("Expected client to have at least %d action implementation but it has %d", minActionCount, len(actions))
	}

	var outputs []*v1beta1.TaskRun
	for _, a := range actions {
		if action, ok := a.(ktesting.CreateAction); ok {
			if output, ok := action.GetObject().(*v1beta1.TaskRun); ok {
				outputs = append(outputs, output)
			}
		}
	}

	if len(outputs) > 0 {
		return outputs
	}
	t.Log("actions", actions)
	t.Fatal("failed to find an action that created a taskrun")
	return nil
}

// getPipelineRunUpdates will look through a set of actions to find all PipelineRun creation actions and return the
// set of them. It will fatal the test if none are found.
func getPipelineRunUpdates(t *testing.T, actions []ktesting.Action) []*v1beta1.PipelineRun {
	t.Helper()
	var outputs []*v1beta1.PipelineRun
	for _, a := range actions {
		if action, ok := a.(ktesting.UpdateAction); ok {
			if output, ok := action.GetObject().(*v1beta1.PipelineRun); ok {
				outputs = append(outputs, output)
			}
		}
	}

	if len(outputs) > 0 {
		return outputs
	}
	t.Fatal("failed to find an action that updated a pipelinerun")
	return nil
}

func TestReconcile(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileWithEmbeddedStatus(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileWithEmbeddedStatus(t *testing.T, embeddedStatus string) {
	// TestReconcile runs "Reconcile" on a PipelineRun with one Task that has not been started yet.
	// It verifies that the TaskRun is created, it checks the resulting API actions, status and events.
	names.TestingSeed()
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-success
  namespace: foo
spec:
  params:
  - name: bar
    value: somethingmorefun
  pipelineRef:
    name: test-pipeline
  resources:
  - name: git-repo
    resourceRef:
      name: some-repo
  - name: best-image
    resourceSpec:
      params:
      - name: url
        value: gcr.io/sven
      type: image
  serviceAccountName: test-sa
`)}
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  params:
  - default: somethingdifferent
    name: pipeline-param
    type: string
  - default: revision
    name: rev-param
    type: string
  - name: bar
    type: string
  resources:
  - name: git-repo
    type: git
  - name: best-image
    type: image
  tasks:
  - name: unit-test-3
    params:
    - name: foo
      value: somethingfun
    - name: bar
      value: $(params.bar)
    - name: templatedparam
      value: $(inputs.workspace.$(params.rev-param))
    - name: contextRunParam
      value: $(context.pipelineRun.name)
    - name: contextPipelineParam
      value: $(context.pipeline.name)
    - name: contextRetriesParam
      value: $(context.pipelineTask.retries)
    resources:
      inputs:
      - name: workspace
        resource: git-repo
      outputs:
      - name: image-to-use
        resource: best-image
      - name: workspace
        resource: git-repo
    runAfter:
    - unit-test-2
    taskRef:
      name: unit-test-task
  - name: unit-test-1
    params:
    - name: foo
      value: somethingfun
    - name: bar
      value: $(params.bar)
    - name: templatedparam
      value: $(inputs.workspace.$(params.rev-param))
    - name: contextRunParam
      value: $(context.pipelineRun.name)
    - name: contextPipelineParam
      value: $(context.pipeline.name)
    - name: contextRetriesParam
      value: $(context.pipelineTask.retries)
    resources:
      inputs:
      - name: workspace
        resource: git-repo
      outputs:
      - name: image-to-use
        resource: best-image
      - name: workspace
        resource: git-repo
    retries: 5
    taskRef:
      name: unit-test-task
  - name: unit-test-2
    resources:
      inputs:
      - from:
        - unit-test-1
        name: workspace
        resource: git-repo
    taskRef:
      name: unit-test-followup-task
  - name: unit-test-cluster-task
    params:
    - name: foo
      value: somethingfun
    - name: bar
      value: $(params.bar)
    - name: templatedparam
      value: $(inputs.workspace.$(params.rev-param))
    - name: contextRunParam
      value: $(context.pipelineRun.name)
    - name: contextPipelineParam
      value: $(context.pipeline.name)
    resources:
      inputs:
      - name: workspace
        resource: git-repo
      outputs:
      - name: image-to-use
        resource: best-image
      - name: workspace
        resource: git-repo
    taskRef:
      kind: ClusterTask
      name: unit-test-cluster-task
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: unit-test-task
  namespace: foo
spec:
  params:
  - name: foo
    type: string
  - name: bar
    type: string
  - name: templatedparam
    type: string
  - name: contextRunParam
    type: string
  - name: contextPipelineParam
    type: string
  - name: contextRetriesParam
    type: string
  resources:
    inputs:
    - name: workspace
      type: git
    outputs:
    - name: image-to-use
      type: image
    - name: workspace
      type: git
`),
		parse.MustParseTask(t, `
metadata:
  name: unit-test-followup-task
  namespace: foo
spec:
  resources:
    inputs:
    - name: workspace
      type: git
`),
	}
	clusterTasks := []*v1beta1.ClusterTask{
		parse.MustParseClusterTask(t, `
metadata:
  name: unit-test-cluster-task
spec:
  params:
  - name: foo
    type: string
  - name: bar
    type: string
  - name: templatedparam
    type: string
  - name: contextRunParam
    type: string
  - name: contextPipelineParam
    type: string
  resources:
    inputs:
    - name: workspace
      type: git
    outputs:
    - name: image-to-use
      type: image
    - name: workspace
      type: git
`),
		parse.MustParseClusterTask(t, `
metadata:
  name: unit-test-followup-task
spec:
  resources:
    inputs:
    - name: workspace
      type: git
`),
	}
	rs := []*resourcev1alpha1.PipelineResource{parse.MustParsePipelineResource(t, `
metadata:
  name: some-repo
  namespace: foo
spec:
  params:
  - name: url
    value: https://github.com/kristoff/reindeer
  type: git
`)}

	// When PipelineResources are created in the cluster, Kubernetes will add a SelfLink. We
	// are using this to differentiate between Resources that we are referencing by Spec or by Ref
	// after we have resolved them.
	rs[0].SelfLink = "some/link"

	d := test.Data{
		PipelineRuns:      prs,
		Pipelines:         ps,
		Tasks:             ts,
		ClusterTasks:      clusterTasks,
		PipelineResources: rs,
		ConfigMaps:        []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-success", wantEvents, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-success-unit-test-1", "foo", "test-pipeline-run-success",
			"test-pipeline", "unit-test-1", false),
		`
spec:
  params:
  - name: foo
    value: somethingfun
  - name: bar
    value: somethingmorefun
  - name: templatedparam
    value: $(inputs.workspace.revision)
  - name: contextRunParam
    value: test-pipeline-run-success
  - name: contextPipelineParam
    value: test-pipeline
  - name: contextRetriesParam
    value: "5"
  resources:
    inputs:
    - name: workspace
      resourceRef:
        apiVersion: tekton.dev/v1alpha1
        name: some-repo
    outputs:
    - name: image-to-use
      paths:
      - /pvc/unit-test-1/image-to-use
      resourceSpec:
        params:
        - name: url
          value: gcr.io/sven
        type: image
    - name: workspace
      paths:
      - /pvc/unit-test-1/workspace
      resourceRef:
        apiVersion: tekton.dev/v1alpha1
        name: some-repo
  serviceAccountName: test-sa
  taskRef:
    name: unit-test-task
    kind: Task
`)
	// ignore IgnoreUnexported ignore both after and before steps fields
	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta, cmpopts.SortSlices(lessTaskResourceBindings)); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}
	// test taskrun is able to recreate correct pipeline-pvc-name
	if expectedTaskRun.GetPipelineRunPVCName() != "test-pipeline-run-success-pvc" {
		t.Errorf("expected to see TaskRun PVC name set to %q created but got %s", "test-pipeline-run-success-pvc", expectedTaskRun.GetPipelineRunPVCName())
	}

	// This PipelineRun is in progress now and the status should reflect that
	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())

	tr1Name := "test-pipeline-run-success-unit-test-1"
	tr2Name := "test-pipeline-run-success-unit-test-cluster-task"

	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 2)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, tr1Name, tr2Name)

	// A PVC should have been created to deal with output -> input linking
	ensurePVCCreated(prt.TestAssets.Ctx, t, clients, expectedTaskRun.GetPipelineRunPVCName(), "foo")

}

// TestReconcile_CustomTask runs "Reconcile" on a PipelineRun with one Custom
// Task reference that has not been run yet.  It verifies that the Run is
// created, it checks the resulting API actions, status and events.
func TestReconcile_CustomTask(t *testing.T) {
	names.TestingSeed()
	const pipelineRunName = "test-pipelinerun"
	const namespace = "namespace"

	simpleCustomTaskPRYAML := `metadata:
  name: test-pipelinerun
  namespace: namespace
spec:
  pipelineSpec:
    tasks:
    - name: custom-task
      params:
      - name: param1
        value: value1
      retries: 3
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
`
	simpleCustomTaskWantRunYAML := `metadata:
  annotations: {}
  labels:
    tekton.dev/memberOf: tasks
    tekton.dev/pipeline: test-pipelinerun
    tekton.dev/pipelineRun: test-pipelinerun
    tekton.dev/pipelineTask: custom-task
  name: test-pipelinerun-custom-task
  namespace: namespace
  ownerReferences:
  - apiVersion: tekton.dev/v1beta1
    blockOwnerDeletion: true
    controller: true
    kind: PipelineRun
    name: test-pipelinerun
spec:
  params:
  - name: param1
    value: value1
  ref:
    apiVersion: example.dev/v0
    kind: Example
  retries: 3
  serviceAccountName: default
`

	tcs := []struct {
		name           string
		pr             *v1beta1.PipelineRun
		wantRun        *v1alpha1.Run
		embeddedStatus string
	}{{
		name:    "simple custom task with taskRef",
		pr:      parse.MustParsePipelineRun(t, simpleCustomTaskPRYAML),
		wantRun: parse.MustParseRun(t, simpleCustomTaskWantRunYAML),
	}, {
		name:           "simple custom task with full embedded status",
		pr:             parse.MustParsePipelineRun(t, simpleCustomTaskPRYAML),
		wantRun:        parse.MustParseRun(t, simpleCustomTaskWantRunYAML),
		embeddedStatus: config.FullEmbeddedStatus,
	}, {
		name:           "simple custom task with minimal embedded status",
		pr:             parse.MustParsePipelineRun(t, simpleCustomTaskPRYAML),
		wantRun:        parse.MustParseRun(t, simpleCustomTaskWantRunYAML),
		embeddedStatus: config.MinimalEmbeddedStatus,
	}, {
		name: "simple custom task with taskSpec",
		pr: parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipelinerun
  namespace: namespace
spec:
  pipelineSpec:
    tasks:
    - name: custom-task
      params:
      - name: param1
        value: value1
      taskSpec:
        apiVersion: example.dev/v0
        kind: Example
        metadata:
          labels:
            test-label: test
        spec:
          field1: 123
          field2: value
`),
		wantRun: mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("test-pipelinerun-custom-task", "namespace", "test-pipelinerun", "test-pipelinerun", "custom-task", false),
			`
spec:
  params:
  - name: param1
    value: value1
  serviceAccountName: default
  spec:
    apiVersion: example.dev/v0
    kind: Example
    metadata:
      labels:
        test-label: test
    spec:
      field1: 123
      field2: value
`),
	}, {
		name: "custom task with workspace",
		pr: parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipelinerun
  namespace: namespace
spec:
  pipelineSpec:
    tasks:
    - name: custom-task
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
      workspaces:
      - name: taskws
        subPath: bar
        workspace: pipelinews
    workspaces:
    - name: pipelinews
  workspaces:
  - name: pipelinews
    persistentVolumeClaim:
      claimName: myclaim
    subPath: foo
`),
		wantRun: mustParseRunWithObjectMeta(t,
			taskRunObjectMetaWithAnnotations("test-pipelinerun-custom-task", "namespace", "test-pipelinerun",
				"test-pipelinerun", "custom-task", false, map[string]string{
					"pipeline.tekton.dev/affinity-assistant": getAffinityAssistantName("pipelinews", pipelineRunName),
				}),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  serviceAccountName: default
  workspaces:
  - name: taskws
    persistentVolumeClaim:
      claimName: myclaim
    subPath: foo/bar
`),
	}}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			embeddedStatus := tc.embeddedStatus
			if embeddedStatus == "" {
				embeddedStatus = config.DefaultEmbeddedStatus
			}
			cms := []*corev1.ConfigMap{withCustomTasks(withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus))}

			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{tc.pr},
				ConfigMaps:   cms,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			wantEvents := []string{
				"Normal Started",
				"Normal Running Tasks Completed: 0",
			}
			reconciledRun, clients := prt.reconcileRun(namespace, pipelineRunName, wantEvents, false)

			actions := clients.Pipeline.Actions()
			if len(actions) < 2 {
				t.Fatalf("Expected client to have at least two action implementation but it has %d", len(actions))
			}

			// Check that the expected Run was created.
			actual := actions[0].(ktesting.CreateAction).GetObject()
			// Ignore the TypeMeta field, because parse.MustParseRun automatically populates it but the "actual" Run won't have it.
			if d := cmp.Diff(tc.wantRun, actual, cmpopts.IgnoreFields(v1alpha1.Run{}, "TypeMeta")); d != "" {
				t.Errorf("expected to see Run created: %s", diff.PrintWantGot(d))
			}

			// This PipelineRun is in progress now and the status should reflect that
			checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())

			verifyRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
			verifyRunStatusesNames(t, embeddedStatus, reconciledRun.Status, tc.wantRun.Name)
		})
	}
}

func TestReconcile_PipelineSpecTaskSpec(t *testing.T) {
	// TestReconcile_PipelineSpecTaskSpec runs "Reconcile" on a PipelineRun that has an embedded PipelineSpec that has an embedded TaskSpec.
	// It verifies that a TaskRun is created, it checks the resulting API actions, status and events.
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcilePipelineSpecTaskSpec(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcilePipelineSpecTaskSpec(t *testing.T, embeddedStatus string) {
	names.TestingSeed()

	prs := []*v1beta1.PipelineRun{
		parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-success
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
`),
	}
	ps := []*v1beta1.Pipeline{
		parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
    - name: unit-test-task-spec
      taskSpec:
        steps:
          - name: mystep
            image: myimage
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		ConfigMaps:   []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-success", wantEvents, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRun := parse.MustParseTaskRun(t, fmt.Sprintf(`
spec:
  taskSpec:
    steps:
      - name: mystep
        image: myimage
  serviceAccountName: %s
`, config.DefaultServiceAccountValue))

	expectedTaskRun.ObjectMeta = taskRunObjectMeta("test-pipeline-run-success-unit-test-task-spec", "foo", "test-pipeline-run-success", "test-pipeline", "unit-test-task-spec", false)
	expectedTaskRun.Spec.Resources = &v1beta1.TaskRunResources{}

	// ignore IgnoreUnexported ignore both after and before steps fields
	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta, cmpopts.SortSlices(func(x, y v1beta1.TaskSpec) bool { return len(x.Steps) == len(y.Steps) })); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}

	// test taskrun is able to recreate correct pipeline-pvc-name
	if expectedTaskRun.GetPipelineRunPVCName() != "test-pipeline-run-success-pvc" {
		t.Errorf("expected to see TaskRun PVC name set to %q created but got %s", "test-pipeline-run-success-pvc", expectedTaskRun.GetPipelineRunPVCName())
	}

	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, "test-pipeline-run-success-unit-test-task-spec")
}

// TestReconcile_InvalidPipelineRuns runs "Reconcile" on several PipelineRuns that are invalid in different ways.
// It verifies that reconcile fails, how it fails and which events are triggered.
func TestReconcile_InvalidPipelineRuns(t *testing.T) {
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task-that-exists
  namespace: foo
`),
		parse.MustParseTask(t, `
metadata:
  name: a-task-that-needs-params
  namespace: foo
spec:
  params:
    - name: some-param
`),
		parse.MustParseTask(t, fmt.Sprintf(`
metadata:
  name: a-task-that-needs-array-params
  namespace: foo
spec:
  params:
    - name: some-param
      type: %s
`, v1beta1.ParamTypeArray)),
		parse.MustParseTask(t, fmt.Sprintf(`
metadata:
  name: a-task-that-needs-object-params
  namespace: foo
spec:
  params:
    - name: some-param
      type: %s
      properties:
        key1: {}
        key2: {}
`, v1beta1.ParamTypeObject)),
		parse.MustParseTask(t, fmt.Sprintf(`
metadata:
  name: a-task-that-needs-a-resource
  namespace: foo
spec:
  resources:
    inputs:
      - name: workspace
        type: %s
`, resourcev1alpha1.PipelineResourceTypeGit)),
	}

	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: pipeline-missing-tasks
  namespace: foo
spec:
  tasks:
    - name: myspecialtask
      taskRef:
        name: sometask
`),
		parse.MustParsePipeline(t, `
metadata:
  name: a-pipeline-without-params
  namespace: foo
spec:
  tasks:
    - name: some-task
      taskRef:
        name: a-task-that-needs-params
`),
		parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: a-fine-pipeline
  namespace: foo
spec:
  tasks:
    - name: some-task
      taskRef:
        name: a-task-that-exists
      resources:
        inputs:
          - name: needed-resource
            resource: a-resource
  resources:
    - name: a-resource
      type: %s
`, resourcev1alpha1.PipelineResourceTypeGit)),
		parse.MustParsePipeline(t, `
metadata:
  name: a-pipeline-that-should-be-caught-by-admission-control
  namespace: foo
spec:
  tasks:
    - name: some-task
      taskRef:
        name: a-task-that-exists
      resources:
        inputs:
          - name: needed-resource
            resource: a-resource
`),
		parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: a-pipeline-with-array-params
  namespace: foo
spec:
  params:
    - name: some-param
      type: %s
  tasks:
    - name: some-task
      taskRef:
        name: a-task-that-needs-array-params
`, v1beta1.ParamTypeArray)),
		parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: a-pipeline-with-object-params
  namespace: foo
spec:
  params:
    - name: some-param
      type: %s
      properties:
        key1: {type: string}
        key2: {type: string}
  tasks:
    - name: some-task
      taskRef:
        name: a-task-that-needs-object-params
`, v1beta1.ParamTypeObject)),
	}

	for _, tc := range []struct {
		name               string
		pipelineRun        *v1beta1.PipelineRun
		reason             string
		hasNoDefaultLabels bool
		permanentError     bool
		wantEvents         []string
	}{{
		name: "invalid-pipeline-shd-be-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: invalid-pipeline
  namespace: foo
spec:
  pipelineRef:
    name: pipeline-not-exist
`),
		reason:             ReasonCouldntGetPipeline,
		hasNoDefaultLabels: true,
		permanentError:     true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed Error retrieving pipeline for pipelinerun",
		},
	}, {
		name: "invalid-pipeline-run-missing-tasks-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipelinerun-missing-tasks
  namespace: foo
spec:
  pipelineRef:
    name: pipeline-missing-tasks
`),
		reason:         ReasonCouldntGetTask,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed Pipeline foo/pipeline-missing-tasks can't be Run",
		},
	}, {
		name: "invalid-pipeline-run-params-dont-exist-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-params-dont-exist
  namespace: foo
spec:
  pipelineRef:
    name: a-pipeline-without-params
`),
		reason:         ReasonFailedValidation,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed invalid input params for task a-task-that-needs-params: missing values",
		},
	}, {
		name: "invalid-pipeline-run-resources-not-bound-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-resources-not-bound
  namespace: foo
spec:
  pipelineRef:
    name: a-fine-pipeline
`),
		reason:         ReasonInvalidBindings,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/pipeline-resources-not-bound doesn't bind Pipeline",
		},
	}, {
		name: "invalid-pipeline-run-missing-resource-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-resources-dont-exist
  namespace: foo
spec:
  pipelineRef:
    name: a-fine-pipeline
  resources:
    - name: a-resource
      resourceRef:
        name: missing-resource
`),
		reason:         ReasonCouldntGetResource,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/pipeline-resources-dont-exist can't be Run; it tries to bind Resources",
		},
	}, {
		name: "invalid-pipeline-missing-declared-resource-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-resources-not-declared
  namespace: foo
spec:
  pipelineRef:
    name: a-pipeline-that-should-be-caught-by-admission-control
`),
		reason:         ReasonFailedValidation,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed Pipeline foo/a-pipeline-that-should-be-caught-by-admission-control can't be Run; it has an invalid spec",
		},
	}, {
		name: "invalid-pipeline-mismatching-parameter-types",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-mismatching-param-type
  namespace: foo
spec:
  pipelineRef:
    name: a-pipeline-with-array-params
  params:
    - name: some-param
      value: stringval
`),
		reason:         ReasonParameterTypeMismatch,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/pipeline-mismatching-param-type parameters have mismatching types",
		},
	}, {
		name: "invalid-pipeline-missing-object-keys",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-missing-object-param-keys
  namespace: foo
spec:
  pipelineRef:
    name: a-pipeline-with-object-params
  params:
    - name: some-param
      value:
        key1: "a"
`),
		reason:         ReasonObjectParameterMissKeys,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/pipeline-missing-object-param-keys parameters is missing object keys required by Pipeline foo/a-pipeline-with-object-params's parameters: PipelineRun missing object keys for parameters",
		},
	}, {
		name: "invalid-embedded-pipeline-resources-bot-bound-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: embedded-pipeline-resources-not-bound
  namespace: foo
spec:
  pipelineSpec:
    tasks:
      - name: some-task
        taskRef:
          name: a-task-that-needs-a-resource
    resources:
      - name: workspace
        type: %s
`, resourcev1alpha1.PipelineResourceTypeGit)),
		reason:         ReasonInvalidBindings,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/embedded-pipeline-resources-not-bound doesn't bind Pipeline",
		},
	}, {
		name: "invalid-embedded-pipeline-bad-name-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: embedded-pipeline-invalid
  namespace: foo
spec:
  pipelineSpec:
    tasks:
      - name: bad-t@$k
        taskRef:
          name: b@d-t@$k
`),
		reason:         ReasonFailedValidation,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed Pipeline foo/embedded-pipeline-invalid can't be Run; it has an invalid spec",
		},
	}, {
		name: "invalid-embedded-pipeline-mismatching-parameter-types",
		pipelineRun: parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: embedded-pipeline-mismatching-param-type
  namespace: foo
spec:
  pipelineSpec:
    params:
      - name: some-param
        type: %s
    tasks:
      - name: some-task
        taskRef:
          name: a-task-that-needs-array-params
  params:
    - name: some-param
      value: stringval
`, v1beta1.ParamTypeArray)),
		reason:         ReasonParameterTypeMismatch,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/embedded-pipeline-mismatching-param-type parameters have mismatching types",
		},
	}, {
		name: "invalid-pipeline-run-missing-params-shd-stop-reconciling",
		pipelineRun: parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: pipelinerun-missing-params
  namespace: foo
spec:
  pipelineSpec:
    params:
      - name: some-param
        type: %s
    tasks:
      - name: some-task
        taskRef:
          name: a-task-that-needs-params
`, v1beta1.ParamTypeString)),
		reason:         ReasonParameterMissing,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo parameters is missing some parameters required by Pipeline pipelinerun-missing-params",
		},
	}, {
		name: "invalid-pipeline-with-invalid-dag-graph",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-invalid-dag-graph
  namespace: foo
spec:
  pipelineSpec:
    tasks:
      - name: dag-task-1
        taskRef:
          name: dag-task-1
        runAfter: [ dag-task-1 ]
`),
		reason:         ReasonInvalidGraph,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo/pipeline-invalid-dag-graph's Pipeline DAG is invalid",
		},
	}, {
		name: "invalid-pipeline-with-invalid-final-tasks-graph",
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline-invalid-final-graph
  namespace: foo
spec:
  pipelineSpec:
    tasks:
      - name: dag-task-1
        taskRef:
          name: taskName
    finally:
      - name: final-task-1
        taskRef:
          name: taskName
      - name: final-task-1
        taskRef:
          name: taskName
`),
		reason:         ReasonInvalidGraph,
		permanentError: true,
		wantEvents: []string{
			"Normal Started",
			"Warning Failed PipelineRun foo's Pipeline DAG is invalid for finally clause",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			cms := []*corev1.ConfigMap{withEnabledAlphaAPIFields(newFeatureFlagsConfigMap())}

			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{tc.pipelineRun},
				Pipelines:    ps,
				Tasks:        ts,
				ConfigMaps:   cms,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			wantEvents := append(tc.wantEvents, "Warning InternalError 1 error occurred") //nolint
			reconciledRun, _ := prt.reconcileRun("foo", tc.pipelineRun.Name, wantEvents, tc.permanentError)

			if reconciledRun.Status.CompletionTime == nil {
				t.Errorf("Expected a CompletionTime on invalid PipelineRun but was nil")
			}

			// Since the PipelineRun is invalid, the status should say it has failed
			checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionFalse, tc.reason)
			if !tc.hasNoDefaultLabels {
				expectedPipelineLabel := reconciledRun.Name
				// Embedded pipelines use the pipelinerun name
				if reconciledRun.Spec.PipelineRef != nil {
					expectedPipelineLabel = reconciledRun.Spec.PipelineRef.Name
				}
				expectedLabels := map[string]string{pipeline.PipelineLabelKey: expectedPipelineLabel}
				if len(reconciledRun.ObjectMeta.Labels) != len(expectedLabels) {
					t.Errorf("Expected labels : %v, got %v", expectedLabels, reconciledRun.ObjectMeta.Labels)
				}
				for k, ev := range expectedLabels {
					if v, ok := reconciledRun.ObjectMeta.Labels[k]; ok {
						if ev != v {
							t.Errorf("Expected labels %s=%s, but was %s", k, ev, v)
						}
					} else {
						t.Errorf("Expected labels %s=%v, but was not present", k, ev)
					}
				}
			}
		})
	}
}

func TestReconcile_InvalidPipelineRunNames(t *testing.T) {
	// TestReconcile_InvalidPipelineRunNames runs "Reconcile" on several PipelineRuns that have invalid names.
	// It verifies that reconcile fails, how it fails and which events are triggered.
	// Note that the code tested here is part of the genreconciler.
	invalidNames := []string{
		"foo/test-pipeline-run-doesnot-exist",
		"test/invalidformat/t",
	}
	tcs := []struct {
		name        string
		pipelineRun string
	}{
		{
			name:        "invalid-pipeline-run-shd-stop-reconciling",
			pipelineRun: invalidNames[0],
		}, {
			name:        "invalid-pipeline-run-name-shd-stop-reconciling",
			pipelineRun: invalidNames[1],
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			testAssets, cancel := getPipelineRunController(t, test.Data{})
			defer cancel()
			c := testAssets.Controller

			err := c.Reconciler.Reconcile(testAssets.Ctx, tc.pipelineRun)
			// No reason to keep reconciling something that doesnt or can't exist
			if err != nil {
				t.Errorf("Did not expect to see error when reconciling invalid PipelineRun but saw %q", err)
			}
		})
	}
}

func TestReconcileOnCompletedPipelineRun(t *testing.T) {
	// TestReconcileOnCompletedPipelineRun runs "Reconcile" on a PipelineRun that already reached completion
	// and that does not have the latest status from TaskRuns yet. It checks that the TaskRun status is updated
	// in the PipelineRun status, that the completion status is not altered, that not error is returned and
	// a successful event is triggered
	taskRunName := "test-pipeline-run-completed-hello-world"
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-completed
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
status:
  conditions:
  - lastTransitionTime: null
    message: All Tasks have completed executing
    reason: Succeeded
    status: "True"
    type: Succeeded
  taskRuns:
    test-pipeline-run-completed-hello-world:
      pipelineTaskName: hello-world-1
`)}
	ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
	ts := []*v1beta1.Task{simpleHelloWorldTask}
	trs := []*v1beta1.TaskRun{createHelloWorldTaskRunWithStatus(t, taskRunName, "foo",
		"test-pipeline-run-completed", "test-pipeline", "",
		apis.Condition{
			Type: apis.ConditionSucceeded,
		})}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Succeeded All Tasks have completed executing",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-completed", wantEvents, false)

	actions := clients.Pipeline.Actions()
	if len(actions) < 2 {
		t.Errorf("# Actions: %d, Actions: %#v", len(actions), actions)
		t.Fatalf("Expected client to have at least two action implementation")
	}

	_ = getPipelineRunUpdates(t, actions)
	pipelineUpdates := 0
	for _, action := range actions {
		if action != nil {
			switch {
			case action.Matches("create", "taskruns"):
				t.Errorf("Expected client to not have created a TaskRun, but it did")
			case action.Matches("update", "pipelineruns"):
				pipelineUpdates++
			}
		}
	}

	if pipelineUpdates != 1 {
		// If only the pipelinerun status changed, we expect one update
		t.Fatalf("Expected client to have updated the pipelinerun twice, but it did %d times", pipelineUpdates)
	}

	// This PipelineRun should still be complete and the status should reflect that
	if reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun status to be complete, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	expectedTaskRunsStatus := make(map[string]*v1beta1.PipelineRunTaskRunStatus)
	expectedTaskRunsStatus[taskRunName] = &v1beta1.PipelineRunTaskRunStatus{
		PipelineTaskName: "hello-world-1",
		Status: &v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{
				Conditions: []apis.Condition{{Type: apis.ConditionSucceeded}},
			},
		},
	}

	if d := cmp.Diff(reconciledRun.Status.TaskRuns, expectedTaskRunsStatus); d != "" {
		t.Fatalf("Expected PipelineRun status to match TaskRun(s) status, but got a mismatch %s", diff.PrintWantGot(d))
	}
}

func newFeatureFlagsConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: system.Namespace(), Name: config.GetFeatureFlagsConfigName()},
		Data:       make(map[string]string),
	}
}

func newDefaultsConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.GetDefaultsConfigName(), Namespace: system.Namespace()},
		Data:       make(map[string]string),
	}
}

func withEnabledAlphaAPIFields(cm *corev1.ConfigMap) *corev1.ConfigMap {
	newCM := cm.DeepCopy()
	newCM.Data[apiFieldsFeatureFlag] = config.AlphaAPIFields
	return newCM
}

func withCustomTasks(cm *corev1.ConfigMap) *corev1.ConfigMap {
	newCM := cm.DeepCopy()
	newCM.Data[customTasksFeatureFlag] = "true"
	return newCM
}

func withOCIBundles(cm *corev1.ConfigMap) *corev1.ConfigMap {
	newCM := cm.DeepCopy()
	newCM.Data[ociBundlesFeatureFlag] = "true"
	return newCM
}

func withEmbeddedStatus(cm *corev1.ConfigMap, flagVal string) *corev1.ConfigMap {
	newCM := cm.DeepCopy()
	newCM.Data[embeddedStatusFeatureFlag] = flagVal
	return newCM
}

func withMaxMatrixCombinationsCount(cm *corev1.ConfigMap, count int) *corev1.ConfigMap {
	newCM := cm.DeepCopy()
	newCM.Data[maxMatrixCombinationsCountFlag] = strconv.Itoa(count)
	return newCM
}

func TestReconcileOnCancelledPipelineRun(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileOnCancelledPipelineRun(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileOnCancelledPipelineRun(t *testing.T, embeddedStatus string) {
	// TestReconcileOnCancelledPipelineRun runs "Reconcile" on a PipelineRun that has been cancelled.
	// It verifies that reconcile is successful, the pipeline status updated and events generated.

	testCases := []struct {
		name       string
		specStatus v1beta1.PipelineRunSpecStatus
		reason     string
	}{
		{
			name:       "cancelled",
			specStatus: v1beta1.PipelineRunSpecStatusCancelled,
			reason:     ReasonCancelled,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			prs := []*v1beta1.PipelineRun{createCancelledPipelineRun(t, "test-pipeline-run-cancelled", tc.specStatus)}
			ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
			ts := []*v1beta1.Task{simpleHelloWorldTask}
			trs := []*v1beta1.TaskRun{createHelloWorldTaskRun(t, "test-pipeline-run-cancelled-hello-world", "foo",
				"test-pipeline-run-cancelled", "test-pipeline")}
			cms := []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)}

			d := test.Data{
				PipelineRuns: prs,
				Pipelines:    ps,
				Tasks:        ts,
				TaskRuns:     trs,
				ConfigMaps:   cms,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			wantEvents := []string{
				"Warning Failed PipelineRun \"test-pipeline-run-cancelled\" was cancelled",
			}
			reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-cancelled", wantEvents, false)
			actions := clients.Pipeline.Actions()

			if reconciledRun.Status.CompletionTime == nil {
				t.Errorf("Expected a CompletionTime on invalid PipelineRun but was nil")
			}

			// This PipelineRun should still be complete and false, and the status should reflect that
			checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionFalse, tc.reason)

			// Check that no TaskRun is created or run
			for _, action := range actions {
				actionType := fmt.Sprintf("%T", action)
				if !(actionType == "testing.UpdateActionImpl" || actionType == "testing.GetActionImpl") {
					t.Errorf("Expected a TaskRun to be get/updated, but it was %s", actionType)
				}
			}
		})
	}
}

func TestReconcileForCustomTaskWithPipelineTaskTimedOut(t *testing.T) {
	names.TestingSeed()
	// TestReconcileForCustomTaskWithPipelineTaskTimedOut runs "Reconcile" on a PipelineRun.
	// It verifies that reconcile is successful, and the individual
	// custom task which has timed out, is patched as cancelled.
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: test
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
`)}
	prName := "test-pipeline-run-custom-task-with-timeout"
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-custom-task-with-timeout
  namespace: test
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
`)}
	runs := []*v1alpha1.Run{mustParseRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-custom-task-with-timeout-hello-world-1", "test", "test-pipeline-run-custom-task-with-timeout",
			"test-pipeline", "hello-world-1", true),
		`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  timeout: 1m0s
status:
  conditions:
  - status: Unknown
    type: Succeeded
  startTime: "2021-12-31T23:58:59Z"
`)}
	cms := []*corev1.ConfigMap{withCustomTasks(newFeatureFlagsConfigMap())}
	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		ConfigMaps:   cms,
		Runs:         runs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0 \\(Failed: 0, Cancelled 0\\), Incomplete: 1, Skipped: 0",
	}
	_, clients := prt.reconcileRun("test", prName, wantEvents, false)

	actions := clients.Pipeline.Actions()
	if len(actions) < 2 {
		t.Fatalf("Expected client to have at least two action implementation but it has %d", len(actions))
	}

	// The patch operation to cancel the run must be executed.
	var got []jsonpatch.Operation
	for _, a := range actions {
		if action, ok := a.(ktesting.PatchAction); ok {
			if a.(ktesting.PatchAction).Matches("patch", "runs") {
				err := json.Unmarshal(action.GetPatch(), &got)
				if err != nil {
					t.Fatalf("Expected to get a patch operation for cancel,"+
						" but got error: %v\n", err)
				}
				break
			}
		}
	}
	var want []jsonpatch.JsonPatchOperation
	if d := cmp.Diff(got, want); d != "" {
		t.Fatalf("Expected no operation, but got %v", got)
	}
}

func TestReconcileForCustomTaskWithPipelineRunTimedOut(t *testing.T) {
	names.TestingSeed()
	// TestReconcileForCustomTaskWithPipelineRunTimedOut runs "Reconcile" on a
	// PipelineRun that has timed out.
	// It verifies that reconcile is successful, and the custom task has also timed
	// out and patched as cancelled.
	for _, tc := range []struct {
		name     string
		timeout  *metav1.Duration
		timeouts *v1beta1.TimeoutFields
	}{{
		name:    "spec.Timeout",
		timeout: &metav1.Duration{Duration: 12 * time.Hour},
	}, {
		name:     "spec.Timeouts.Pipeline",
		timeouts: &v1beta1.TimeoutFields{Pipeline: &metav1.Duration{Duration: 12 * time.Hour}},
	}} {
		t.Run(tc.name, func(*testing.T) {
			ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: test
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
`)}

			prName := "test-pipeline-run-custom-task"
			prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-custom-task
  namespace: test
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
status:
  conditions:
  - message: running...
    reason: Running
    status: Unknown
    type: Succeeded
  startTime: "2021-12-31T00:00:00Z"
  runs:
    test-pipeline-run-custom-task-hello-world-1:
      pipelineTaskName: hello-world-1
      status:
        conditions:
        - status: Unknown
          type: Succeeded
`)}
			prs[0].Spec.Timeout = tc.timeout
			prs[0].Spec.Timeouts = tc.timeouts

			runs := []*v1alpha1.Run{mustParseRunWithObjectMeta(t,
				taskRunObjectMeta("test-pipeline-run-custom-task-hello-world-1", "test", "test-pipeline-run-custom-task",
					"test-pipeline", "hello-world-1", true),
				`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
status:
  conditions:
  - status: Unknown
    type: Succeeded
  startTime: "2021-12-31T11:58:59Z"
  creationTime: "2021-12-31T11:58:58Z"
`)}

			cms := []*corev1.ConfigMap{withCustomTasks(newFeatureFlagsConfigMap())}
			d := test.Data{
				PipelineRuns: prs,
				Pipelines:    ps,
				ConfigMaps:   cms,
				Runs:         runs,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			wantEvents := []string{
				fmt.Sprintf("Warning Failed PipelineRun \"%s\" failed to finish within \"12h0m0s\"", prName),
			}

			reconciledRun, clients := prt.reconcileRun("test", prName, wantEvents, false)

			if reconciledRun.Status.CompletionTime == nil {
				t.Errorf("Expected a CompletionTime on already timedout PipelineRun but was nil")
			}

			// The PipelineRun should be timed out.
			if reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason != "PipelineRunTimeout" {
				t.Errorf("Expected PipelineRun to be timed out, but condition reason is %s",
					reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
			}

			actions := clients.Pipeline.Actions()
			if len(actions) < 2 {
				t.Fatalf("Expected client to have at least two action implementation but it has %d", len(actions))
			}

			// The patch operation to cancel the run must be executed.
			var got []jsonpatch.Operation
			for _, a := range actions {
				if action, ok := a.(ktesting.PatchAction); ok {
					if a.(ktesting.PatchAction).Matches("patch", "runs") {
						err := json.Unmarshal(action.GetPatch(), &got)
						if err != nil {
							t.Fatalf("Expected to get a patch operation for cancel,"+
								" but got error: %v\n", err)
						}
						break
					}
				}
			}
			want := []jsonpatch.JsonPatchOperation{{
				Operation: "add",
				Path:      "/spec/status",
				Value:     string(v1alpha1.RunReasonCancelled),
			}, {
				Operation: "add",
				Path:      "/spec/statusMessage",
				Value:     string(v1alpha1.RunCancelledByPipelineTimeoutMsg),
			}}
			if d := cmp.Diff(got, want); d != "" {
				t.Fatalf("Expected RunCancelled patch operation, but got a mismatch %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestReconcileOnCancelledRunFinallyPipelineRun(t *testing.T) {
	testCases := []struct {
		name        string
		embeddedVal string
	}{
		{
			name:        "default embedded status",
			embeddedVal: config.DefaultEmbeddedStatus,
		},
		{
			name:        "full embedded status",
			embeddedVal: config.FullEmbeddedStatus,
		},
		{
			name:        "both embedded status",
			embeddedVal: config.BothEmbeddedStatus,
		},
		{
			name:        "minimal embedded status",
			embeddedVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileOnCancelledRunFinallyPipelineRun(t, tc.embeddedVal)
		})
	}
}

func runTestReconcileOnCancelledRunFinallyPipelineRun(t *testing.T, embeddedStatus string) {
	// TestReconcileOnCancelledRunFinallyPipelineRun runs "Reconcile" on a PipelineRun that has been gracefully cancelled.
	// It verifies that reconcile is successful, the pipeline status updated and events generated.
	prs := []*v1beta1.PipelineRun{createCancelledPipelineRun(t, "test-pipeline-run-cancelled-run-finally", v1beta1.PipelineRunSpecStatusCancelledRunFinally)}
	ps := []*v1beta1.Pipeline{helloWorldPipelineWithRunAfter(t)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}
	cms := []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)}
	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Warning Failed PipelineRun \"test-pipeline-run-cancelled-run-finally\" was cancelled",
	}
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-cancelled-run-finally", wantEvents, false)

	if reconciledRun.Status.CompletionTime == nil {
		t.Errorf("Expected a CompletionTime on invalid PipelineRun but was nil")
	}

	// This PipelineRun should still be complete and false, and the status should reflect that
	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsFalse() {
		t.Errorf("Expected PipelineRun status to be complete and false, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// There should be no task runs triggered for the pipeline tasks
	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 0)

	expectedSkippedTasks := []v1beta1.SkippedTask{{
		Name:   "hello-world-1",
		Reason: v1beta1.GracefullyCancelledSkip,
	}, {
		Name:   "hello-world-2",
		Reason: v1beta1.GracefullyCancelledSkip,
	}}

	if d := cmp.Diff(expectedSkippedTasks, reconciledRun.Status.SkippedTasks); d != "" {
		t.Fatalf("Didn't get the expected list of skipped tasks. Diff: %s", diff.PrintWantGot(d))
	}
}

func TestReconcileOnCancelledRunFinallyPipelineRunWithFinalTask(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileOnCancelledRunFinallyPipelineRunWithFinalTask(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileOnCancelledRunFinallyPipelineRunWithFinalTask(t *testing.T, embeddedStatus string) {
	// TestReconcileOnCancelledRunFinallyPipelineRunWithFinalTask runs "Reconcile" on a PipelineRun that has been gracefully cancelled.
	// It verifies that reconcile is successful, final tasks run, the pipeline status updated and events generated.
	prs := []*v1beta1.PipelineRun{createCancelledPipelineRun(t, "test-pipeline-run-cancelled-run-finally", v1beta1.PipelineRunSpecStatusCancelledRunFinally)}
	ps := []*v1beta1.Pipeline{
		parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
    - name: hello-world-1
      taskRef:
        name: hello-world
    - name: hello-world-2
      taskRef:
        name: hello-world
      runAfter:
        - hello-world-1
  finally:
    - name: final-task-1
      taskRef:
        name: some-task
`),
	}
	ts := []*v1beta1.Task{
		simpleHelloWorldTask,
		simpleSomeTask,
	}
	cms := []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
	}
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-cancelled-run-finally", wantEvents, false)

	if reconciledRun.Status.CompletionTime != nil {
		t.Errorf("Expected a CompletionTime to be nil on incomplete PipelineRun but was %v", reconciledRun.Status.CompletionTime)
	}

	// This PipelineRun should still be complete and false, and the status should reflect that
	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun status to be complete and unknown, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// There should be exactly one task run triggered for the "final-task-1" final task
	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, "test-pipeline-run-cancelled-run-finally-final-task-1")
}

func TestReconcileOnCancelledRunFinallyPipelineRunWithRunningFinalTask(t *testing.T) {
	// TestReconcileOnCancelledRunFinallyPipelineRunWithRunningFinalTask runs "Reconcile" on a PipelineRun that has been gracefully cancelled.
	// It verifies that reconcile is successful and completed tasks and running final tasks are left untouched.
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-cancelled-run-finally
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  status: CancelledRunFinally
status:
  startTime: "2022-01-01T00:00:00Z"
  taskRuns:
    test-pipeline-run-cancelled-run-finally-final-task:
      pipelineTaskName: final-task-1
    test-pipeline-run-cancelled-run-finally-hello-world:
      pipelineTaskName: hello-world-1
      status:
        conditions:
        - lastTransitionTime: null
          status: "True"
          type: Succeeded
`)}
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  finally:
  - name: final-task-1
    taskRef:
      name: some-task
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
`)}
	ts := []*v1beta1.Task{
		simpleHelloWorldTask,
		simpleSomeTask,
	}
	trs := []*v1beta1.TaskRun{
		createHelloWorldTaskRunWithStatus(t, "test-pipeline-run-cancelled-run-finally-hello-world", "foo",
			"test-pipeline-run-cancelled-run-finally", "test-pipeline", "my-pod-name",
			apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			}),
		createHelloWorldTaskRun(t, "test-pipeline-run-cancelled-run-finally-final-task", "foo",
			"test-pipeline-run-cancelled-run-finally", "test-pipeline"),
	}
	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-cancelled-run-finally", wantEvents, false)

	if reconciledRun.Status.CompletionTime != nil {
		t.Errorf("Expected a CompletionTime to be nil on incomplete PipelineRun but was %v", reconciledRun.Status.CompletionTime)
	}

	// This PipelineRun should still be complete and unknown, and the status should reflect that
	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun status to be complete and unknown, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// There should be 2 task runs, one for already completed "hello-world-1" task and one for the "final-task-1" final task
	if len(reconciledRun.Status.TaskRuns) != 2 {
		t.Errorf("Expected PipelineRun status to have 2 task runs, but was %v", len(reconciledRun.Status.TaskRuns))
	}

	actions := clients.Pipeline.Actions()
	patchActions := make([]ktesting.PatchAction, 0)
	for _, action := range actions {
		if patchAction, ok := action.(ktesting.PatchAction); ok {
			patchActions = append(patchActions, patchAction)
		}
	}
	if len(patchActions) != 0 {
		t.Errorf("Expected no patch actions, but was %v", len(patchActions))
	}
}

func TestReconcileOnCancelledRunFinallyPipelineRunWithFinalTaskAndRetries(t *testing.T) {
	// TestReconcileOnCancelledRunFinallyPipelineRunWithFinalTaskAndRetries runs "Reconcile" on a PipelineRun that has
	// been gracefully cancelled. It verifies that reconcile is successful, the pipeline status updated and events generated.

	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileOnCancelledRunFinallyPipelineRunWithFinalTaskAndRetries(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileOnCancelledRunFinallyPipelineRunWithFinalTaskAndRetries(t *testing.T, embeddedStatus string) {
	// Pipeline has a DAG task "hello-world-1" and Finally task "hello-world-2"
	ps := []*v1beta1.Pipeline{{
		ObjectMeta: baseObjectMeta("test-pipeline", "foo"),
		Spec: v1beta1.PipelineSpec{
			Tasks: []v1beta1.PipelineTask{{
				Name: "hello-world-1",
				TaskRef: &v1beta1.TaskRef{
					Name: "hello-world",
				},
				Retries: 2,
			}},
			Finally: []v1beta1.PipelineTask{{
				Name: "hello-world-2",
				TaskRef: &v1beta1.TaskRef{
					Name: "hello-world",
				},
			}},
		},
	}}

	// PipelineRun has been gracefully cancelled, and it has a TaskRun for DAG task "hello-world-1" that has failed
	// with reason of cancellation
	prs := []*v1beta1.PipelineRun{{
		ObjectMeta: baseObjectMeta("test-pipeline-run-cancelled-run-finally", "foo"),
		Spec: v1beta1.PipelineRunSpec{
			PipelineRef:        &v1beta1.PipelineRef{Name: "test-pipeline"},
			ServiceAccountName: "test-sa",
			Status:             v1beta1.PipelineRunSpecStatusCancelledRunFinally,
		},
		Status: v1beta1.PipelineRunStatus{
			PipelineRunStatusFields: v1beta1.PipelineRunStatusFields{},
		},
	}}

	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		prs[0].Status.ChildReferences = append(prs[0].Status.ChildReferences, v1beta1.ChildStatusReference{
			TypeMeta: runtime.TypeMeta{
				APIVersion: v1beta1.SchemeGroupVersion.String(),
				Kind:       "TaskRun",
			},
			Name:             "test-pipeline-run-cancelled-run-finally-hello-world",
			PipelineTaskName: "hello-world-1",
		})
	}
	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		prs[0].Status.TaskRuns = map[string]*v1beta1.PipelineRunTaskRunStatus{
			"test-pipeline-run-cancelled-run-finally-hello-world": {
				PipelineTaskName: "hello-world-1",
				Status: &v1beta1.TaskRunStatus{
					Status: duckv1beta1.Status{
						Conditions: []apis.Condition{{
							Type:   apis.ConditionSucceeded,
							Status: corev1.ConditionFalse,
							Reason: v1beta1.TaskRunReasonCancelled.String(),
						}},
					},
				},
			},
		}
	}

	// TaskRun exists for DAG task "hello-world-1" that has failed with reason of cancellation
	trs := []*v1beta1.TaskRun{createHelloWorldTaskRunWithStatus(t, "test-pipeline-run-cancelled-run-finally-hello-world", "foo",
		"test-pipeline-run-cancelled-run-finally", "test-pipeline", "my-pod-name",
		apis.Condition{
			Type:   apis.ConditionSucceeded,
			Status: corev1.ConditionFalse,
			Reason: v1beta1.TaskRunSpecStatusCancelled,
		})}

	ts := []*v1beta1.Task{simpleHelloWorldTask}
	cms := []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		TaskRuns:     trs,
		Tasks:        ts,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal CancelledRunningFinally Tasks Completed: 1 \\(Failed: 0, Cancelled 1\\), Incomplete: 1, Skipped: 0",
	}
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-cancelled-run-finally", wantEvents, false)

	// This PipelineRun should still be running to execute the finally task, and the status should reflect that
	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun status to be running to execute the finally task, but was %v",
			reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// There should be two task runs (failed dag task and one triggered for the finally task)
	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 2)
}

func TestReconcileTaskResolutionError(t *testing.T) {
	ts := []*v1beta1.Task{
		simpleHelloWorldTask,
	}
	ptName := "hello-world-1"
	prName := "test-pipeline-fails-task-resolution"
	prs := []*v1beta1.PipelineRun{
		parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  status: %s
status:
  conditions:
  - type: Succeeded
    status: Unknown
    reason: Running
    message: "running..."
  taskRuns:
    %s%s:
      pipelineTaskName: %s
      status: {}
      startTime: %s
`, prName, v1beta1.PipelineRunSpecStatusCancelledRunFinally, ptName, prName, ptName, now.Format(time.RFC3339))),
	}

	trs := []*v1beta1.TaskRun{
		getTaskRun(
			t,
			"test-pipeline-fails-task-resolutionhello-world-1",
			prName,
			"test-pipeline",
			"hello-world",
			corev1.ConditionUnknown,
		),
	}
	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    []*v1beta1.Pipeline{simpleHelloWorldPipeline},
		Tasks:        ts,
		TaskRuns:     trs,
		ConfigMaps:   []*corev1.ConfigMap{},
	}
	testAssets, cancel := getPipelineRunController(t, d)
	defer cancel()
	c := testAssets.Controller
	clients := testAssets.Clients

	failingReactorActivated := true
	// Create an error when the Pipeline client attempts to resolve the Task
	clients.Pipeline.PrependReactor("*", "tasks", func(action ktesting.Action) (bool, runtime.Object, error) {
		return failingReactorActivated, nil, errors.New("etcdserver: leader changed")
	})
	err := c.Reconciler.Reconcile(testAssets.Ctx, "foo/test-pipeline-fails-task-resolution")
	if err == nil {
		t.Errorf("Expected error returned from reconcile after failing to cancel TaskRun but saw none!")
	}

	if controller.IsPermanentError(err) {
		t.Errorf("Expected non-permanent error reconciling PipelineRun but got permanent error: %v", err)
	}

	// Check that the PipelineRun is still running with correct error message
	reconciledRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(testAssets.Ctx, "test-pipeline-fails-task-resolution", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Somehow had error getting reconciled run out of fake client: %s", err)
	}

	// The PipelineRun should not be cancelled since the error was retryable
	condition := reconciledRun.Status.GetCondition(apis.ConditionSucceeded)
	if !condition.IsUnknown() {
		t.Errorf("Expected PipelineRun to still be running but succeeded condition is %v", condition.Status)
	}

	failingReactorActivated = false
	err = c.Reconciler.Reconcile(testAssets.Ctx, "foo/test-pipeline-fails-task-resolution")
	if err != nil {
		if ok, _ := controller.IsRequeueKey(err); !ok {
			t.Errorf("unexpected error in PipelineRun reconciliation: %v", err)
		}
	}
	condition = reconciledRun.Status.GetCondition(apis.ConditionSucceeded)
	if !condition.IsUnknown() {
		t.Errorf("Expected PipelineRun to still be running but succeeded condition is %v", condition.Status)
	}
}

func TestReconcileOnStoppedPipelineRun(t *testing.T) {
	basePRYAML := fmt.Sprintf(`
metadata:
  name: "test-pipeline-run-stopped-run-finally"
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  status: %s
status:
  startTime: %s`, v1beta1.PipelineRunSpecStatusStoppedRunFinally, now.Format(time.RFC3339))

	testCases := []struct {
		name                 string
		pipeline             *v1beta1.Pipeline
		taskRuns             []*v1beta1.TaskRun
		initialTaskRunStatus map[string]*v1beta1.PipelineRunTaskRunStatus
		expectedEvents       []string
		hasNilCompletionTime bool
		isFailed             bool
		trInStatusCount      int
		skippedTasks         []v1beta1.SkippedTask
	}{
		{
			name:                 "stopped PipelineRun",
			pipeline:             simpleHelloWorldPipeline,
			taskRuns:             nil,
			initialTaskRunStatus: nil,
			expectedEvents:       []string{"Warning Failed PipelineRun \"test-pipeline-run-stopped-run-finally\" was cancelled"},
			hasNilCompletionTime: false,
			isFailed:             true,
			trInStatusCount:      0,
			skippedTasks:         []v1beta1.SkippedTask{{Name: "hello-world-1", Reason: v1beta1.GracefullyStoppedSkip}},
		}, {
			name:     "with running task",
			pipeline: simpleHelloWorldPipeline,
			taskRuns: []*v1beta1.TaskRun{getTaskRun(
				t,
				"test-pipeline-run-stopped-run-finally-hello-world",
				"test-pipeline-run-stopped-run-finally",
				"test-pipeline",
				"hello-world",
				corev1.ConditionUnknown,
			)},
			initialTaskRunStatus: map[string]*v1beta1.PipelineRunTaskRunStatus{
				"test-pipeline-run-stopped-run-finally-hello-world": {
					PipelineTaskName: "hello-world-1",
					Status:           &v1beta1.TaskRunStatus{},
				},
			},
			expectedEvents:       []string{"Normal Started"},
			hasNilCompletionTime: true,
			isFailed:             false,
			trInStatusCount:      1,
			skippedTasks:         nil,
		}, {
			name:     "with completed task",
			pipeline: helloWorldPipelineWithRunAfter(t),
			taskRuns: []*v1beta1.TaskRun{getTaskRun(
				t,
				"test-pipeline-run-stopped-run-finally-hello-world",
				"test-pipeline-run-stopped-run-finally",
				"test-pipeline",
				"hello-world",
				corev1.ConditionTrue,
			)},
			initialTaskRunStatus: map[string]*v1beta1.PipelineRunTaskRunStatus{
				"test-pipeline-run-stopped-run-finally-hello-world": {
					PipelineTaskName: "hello-world-1",
					Status:           &v1beta1.TaskRunStatus{},
				},
			},
			expectedEvents:       []string{"Warning Failed PipelineRun \"test-pipeline-run-stopped-run-finally\" was cancelled"},
			hasNilCompletionTime: false,
			isFailed:             true,
			trInStatusCount:      1,
			skippedTasks:         []v1beta1.SkippedTask{{Name: "hello-world-2", Reason: v1beta1.GracefullyStoppedSkip}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pr := parse.MustParsePipelineRun(t, basePRYAML)
			if tc.initialTaskRunStatus != nil {
				pr.Status.TaskRuns = tc.initialTaskRunStatus
			}
			ps := []*v1beta1.Pipeline{tc.pipeline}
			ts := []*v1beta1.Task{simpleHelloWorldTask}
			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{pr},
				Pipelines:    ps,
				Tasks:        ts,
				TaskRuns:     tc.taskRuns,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			var wantEvents []string

			for _, we := range tc.expectedEvents {
				wantEvents = append(wantEvents, we)
			}

			reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-stopped-run-finally", wantEvents, false)

			if (reconciledRun.Status.CompletionTime == nil) != tc.hasNilCompletionTime {
				t.Errorf("Expected CompletionTime == nil to be %t on invalid PipelineRun but was %t", tc.hasNilCompletionTime, reconciledRun.Status.CompletionTime == nil)
			}

			if tc.isFailed && !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsFalse() {
				t.Errorf("Expected PipelineRun status to be complete and false, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
			} else if !tc.isFailed && !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
				t.Errorf("Expected PipelineRun status to be complete and unknown, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
			}

			if len(reconciledRun.Status.TaskRuns) != tc.trInStatusCount {
				t.Fatalf("Expected %d TaskRuns in status but got %d", tc.trInStatusCount, len(reconciledRun.Status.TaskRuns))
			}

			if d := cmp.Diff(tc.skippedTasks, reconciledRun.Status.SkippedTasks); d != "" {
				t.Fatalf("Didn't get the expected list of skipped tasks. Diff: %s", diff.PrintWantGot(d))
			}

			actions := clients.Pipeline.Actions()
			patchCount := 0
			for _, action := range actions {
				if _, ok := action.(ktesting.PatchAction); ok {
					patchCount++
				}
			}
			if patchCount != 0 {
				t.Errorf("Expected no patch action, but was %v", patchCount)
			}
		})
	}
}

func TestReconcileOnPendingPipelineRun(t *testing.T) {
	// TestReconcileOnPendingPipelineRun runs "Reconcile" on a PipelineRun that is pending.
	// It verifies that reconcile is successful, the pipeline status updated and events generated.
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-pending
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  status: PipelineRunPending
`)}
	ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
	var ts []*v1beta1.Task
	var trs []*v1beta1.TaskRun

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	var wantEvents []string
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-pending", wantEvents, false)

	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonPending.String())

	if reconciledRun.Status.StartTime != nil {
		t.Errorf("Start time should be nil, not: %s", reconciledRun.Status.StartTime)
	}
}

func TestReconcileWithTimeoutDeprecated(t *testing.T) {
	// TestReconcileWithTimeoutDeprecated runs "Reconcile" on a PipelineRun that has timed out.
	// It verifies that reconcile is successful, no TaskRun is created, the PipelineTask is marked as skipped, and the
	// pipeline status updated and events generated.
	ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-with-timeout
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  timeout: 12h0m0s
status:
  startTime: "2021-12-31T00:00:00Z"
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Warning Failed PipelineRun \"test-pipeline-run-with-timeout\" failed to finish within \"12h0m0s\"",
	}
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-with-timeout", wantEvents, false)

	if reconciledRun.Status.CompletionTime == nil {
		t.Errorf("Expected a CompletionTime on invalid PipelineRun but was nil")
	}

	// The PipelineRun should be timed out.
	if reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason != "PipelineRunTimeout" {
		t.Errorf("Expected PipelineRun to be timed out, but condition reason is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// Check that there is a skipped task for the expected reason
	if len(reconciledRun.Status.SkippedTasks) != 1 {
		t.Errorf("expected one skipped task, found %d", len(reconciledRun.Status.SkippedTasks))
	} else if reconciledRun.Status.SkippedTasks[0].Reason != v1beta1.PipelineTimedOutSkip {
		t.Errorf("expected skipped reason to be '%s', but was '%s", v1beta1.PipelineTimedOutSkip, reconciledRun.Status.SkippedTasks[0].Reason)
	}
}

func TestReconcileWithTimeouts_Pipeline(t *testing.T) {
	// TestReconcileWithTimeouts_Pipeline runs "Reconcile" on a PipelineRun that has timed out.
	// It verifies that reconcile is successful, no TaskRun is created, the PipelineTask is marked as skipped, and the
	// pipeline status updated and events generated.
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
  - name: hello-world-2
    taskRef:
      name: hello-world
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-with-timeout
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  timeouts:
    pipeline: 12h0m0s
status:
  startTime: "2021-12-31T00:00:00Z"
  taskRuns:
    test-pipeline-run-with-timeout-hello-world-1:
      pipelineTaskName: hello-world-1
      status:
        conditions:
        - lastTransitionTime: null
          status: "Unknown"
          type: Succeeded
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t, taskRunObjectMeta("test-pipeline-run-with-timeout-hello-world-1", "foo", "test-pipeline-run-with-timeout",
		"test-pipeline", "hello-world-1", false), `
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
    kind: Task
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Warning Failed PipelineRun \"test-pipeline-run-with-timeout\" failed to finish within \"12h0m0s\"",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-with-timeout", wantEvents, false)

	if reconciledRun.Status.CompletionTime == nil {
		t.Errorf("Expected a CompletionTime on invalid PipelineRun but was nil")
	}

	// The PipelineRun should be timed out.
	if reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason != "PipelineRunTimeout" {
		t.Errorf("Expected PipelineRun to be timed out, but condition reason is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// Check that there is a skipped task for the expected reason
	if len(reconciledRun.Status.SkippedTasks) != 1 {
		t.Errorf("expected one skipped task, found %d", len(reconciledRun.Status.SkippedTasks))
	} else if reconciledRun.Status.SkippedTasks[0].Reason != v1beta1.PipelineTimedOutSkip {
		t.Errorf("expected skipped reason to be '%s', but was '%s", v1beta1.PipelineTimedOutSkip, reconciledRun.Status.SkippedTasks[0].Reason)
	}

	updatedTaskRun, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").Get(context.Background(), trs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("error getting updated TaskRun: %#v", err)
	}

	if updatedTaskRun.Spec.Status != v1beta1.TaskRunSpecStatusCancelled {
		t.Errorf("expected existing TaskRun Spec.Status to be set to %s, but was %s", v1beta1.TaskRunSpecStatusCancelled, updatedTaskRun.Spec.Status)
	}
	if updatedTaskRun.Spec.StatusMessage != v1beta1.TaskRunCancelledByPipelineTimeoutMsg {
		t.Errorf("expected existing TaskRun Spec.StatusMessage to be set to %s, but was %s", v1beta1.TaskRunCancelledByPipelineTimeoutMsg, updatedTaskRun.Spec.StatusMessage)
	}
}

func TestReconcileWithTimeouts_Tasks(t *testing.T) {
	// TestReconcileWithTimeouts_Tasks runs "Reconcile" on a PipelineRun with timeouts.tasks configured.
	// It verifies that reconcile is successful, no TaskRun is created, the PipelineTask is marked as skipped, and the
	// pipeline status updated and events generated.
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
  - name: hello-world-2
    taskRef:
      name: hello-world
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-with-timeout
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  timeouts:
    tasks: 2m
status:
  startTime: "2021-12-31T23:55:00Z"
  taskRuns:
    test-pipeline-run-with-timeout-hello-world-1:
      pipelineTaskName: hello-world-1
      status:
        conditions:
        - lastTransitionTime: null
          status: "Unknown"
          type: Succeeded
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t, taskRunObjectMeta("test-pipeline-run-with-timeout-hello-world-1", "foo", "test-pipeline-run-with-timeout",
		"test-pipeline", "hello-world-1", false), `
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
    kind: Task
status:
  conditions:
  - lastTransitionTime: null
    status: "Unknown"
    type: Succeeded
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-with-timeout", wantEvents, false)

	if reconciledRun.Status.CompletionTime != nil {
		t.Errorf("Expected nil CompletionTime on PipelineRun but was %s", reconciledRun.Status.CompletionTime)
	}

	// The PipelineRun should be running.
	if reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason != v1beta1.PipelineRunReasonRunning.String() {
		t.Errorf("Expected PipelineRun to be running, but condition reason is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason)
	}

	// Check that there is a skipped task for the expected reason
	if len(reconciledRun.Status.SkippedTasks) != 1 {
		t.Errorf("expected one skipped task, found %d", len(reconciledRun.Status.SkippedTasks))
	} else if reconciledRun.Status.SkippedTasks[0].Reason != v1beta1.TasksTimedOutSkip {
		t.Errorf("expected skipped reason to be '%s', but was '%s", v1beta1.TasksTimedOutSkip, reconciledRun.Status.SkippedTasks[0].Reason)
	}

	updatedTaskRun, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").Get(context.Background(), trs[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("error getting updated TaskRun: %#v", err)
	}

	if updatedTaskRun.Spec.Status != v1beta1.TaskRunSpecStatusCancelled {
		t.Errorf("expected existing TaskRun Spec.Status to be set to %s, but was %s", v1beta1.TaskRunSpecStatusCancelled, updatedTaskRun.Spec.Status)
	}
	if updatedTaskRun.Spec.StatusMessage != v1beta1.TaskRunCancelledByPipelineTimeoutMsg {
		t.Errorf("expected existing TaskRun Spec.StatusMessage to be set to %s, but was %s", v1beta1.TaskRunCancelledByPipelineTimeoutMsg, updatedTaskRun.Spec.StatusMessage)
	}
}

func TestReconcileWithTimeouts_Finally(t *testing.T) {
	// TestReconcileWithTimeouts_Finally runs "Reconcile" on a PipelineRun with timeouts.finally configured.
	// It verifies that reconcile is successful, no TaskRun is created, the PipelineTask is marked as skipped, and the
	// pipeline status updated and events generated.
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline-with-finally
  namespace: foo
spec:
  finally:
  - name: finaltask-1
    taskRef:
      name: hello-world
  - name: finaltask-2
    taskRef:
      name: hello-world
  tasks:
  - name: task1
    taskRef:
      name: hello-world
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-with-timeout
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline-with-finally
  serviceAccountName: test-sa
  timeouts:
    finally: 15m
status:
  finallyStartTime: "2021-12-31T23:44:59Z"
  startTime: "2021-12-31T23:40:00Z"
  taskRuns:
    test-pipeline-run-with-timeout-hello-world:
      pipelineTaskName: task1
      status:
        conditions:
        - lastTransitionTime: null
          status: "True"
          type: Succeeded
    test-pipeline-run-with-timeout-finaltask-1:
      pipelineTaskName: finaltask-1
      status:
        conditions:
        - lastTransitionTime: null
          status: "Unknown"
          type: Succeeded
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}
	trs := []*v1beta1.TaskRun{
		getTaskRun(
			t,
			"test-pipeline-run-with-timeout-hello-world",
			prs[0].Name,
			ps[0].Name,
			"hello-world",
			corev1.ConditionTrue,
		),
		mustParseTaskRunWithObjectMeta(t, taskRunObjectMeta("test-pipeline-run-with-timeout-finaltask-1", "foo", "test-pipeline-run-with-timeout",
			"test-pipeline", "finaltask-1", false), `
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
    kind: Task
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-with-timeout", wantEvents, false)

	if reconciledRun.Status.CompletionTime != nil {
		t.Errorf("Expected a nil CompletionTime on running PipelineRun but was %s", reconciledRun.Status.CompletionTime.String())
	}

	// The PipelineRun should be running.
	if reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason != v1beta1.PipelineRunReasonRunning.String() {
		t.Errorf("Expected PipelineRun to be running, but condition reason is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded).Reason)
	}

	// Check that there is a skipped task for the expected reason
	if len(reconciledRun.Status.SkippedTasks) != 1 {
		t.Errorf("expected one skipped task, found %d", len(reconciledRun.Status.SkippedTasks))
	} else if reconciledRun.Status.SkippedTasks[0].Reason != v1beta1.FinallyTimedOutSkip {
		t.Errorf("expected skipped reason to be '%s', but was '%s", v1beta1.FinallyTimedOutSkip, reconciledRun.Status.SkippedTasks[0].Reason)
	}

	updatedTaskRun, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").Get(context.Background(), trs[1].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("error getting updated TaskRun: %#v", err)
	}

	if updatedTaskRun.Spec.Status != v1beta1.TaskRunSpecStatusCancelled {
		t.Errorf("expected existing TaskRun Spec.Status to be set to %s, but was %s", v1beta1.TaskRunSpecStatusCancelled, updatedTaskRun.Spec.Status)
	}
	if updatedTaskRun.Spec.StatusMessage != v1beta1.TaskRunCancelledByPipelineTimeoutMsg {
		t.Errorf("expected existing TaskRun Spec.StatusMessage to be set to %s, but was %s", v1beta1.TaskRunCancelledByPipelineTimeoutMsg, updatedTaskRun.Spec.StatusMessage)
	}
}

func TestReconcileWithoutPVC(t *testing.T) {
	// TestReconcileWithoutPVC runs "Reconcile" on a PipelineRun that has two unrelated tasks.
	// It verifies that reconcile is successful and that no PVC is created
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
  - name: hello-world-2
    taskRef:
      name: hello-world
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run", []string{}, false)
	actions := clients.Pipeline.Actions()

	// Check that the expected TaskRun was created
	for _, a := range actions {
		if ca, ok := a.(ktesting.CreateAction); ok {
			obj := ca.GetObject()
			if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				t.Errorf("Did not expect to see a PVC created when no resources are linked. %s was created", pvc)
			}
		}
	}

	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun to be running, but condition status is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}
}

func TestReconcileCancelledFailsTaskRunCancellation(t *testing.T) {
	prName := "test-pipeline-fails-to-cancel"

	testCases := []struct {
		name       string
		specStatus v1beta1.PipelineRunSpecStatus
	}{
		{
			name:       "cancelled",
			specStatus: v1beta1.PipelineRunSpecStatusCancelled,
		},
		{
			name:       "cancelled run finally",
			specStatus: v1beta1.PipelineRunSpecStatusCancelledRunFinally,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			// TestReconcileCancelledFailsTaskRunCancellation runs "Reconcile" on a PipelineRun with a single TaskRun.
			// The TaskRun cannot be cancelled. Check that the pipelinerun cancel fails, that reconcile fails and
			// an event is generated
			names.TestingSeed()
			prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: test-pipeline-fails-to-cancel
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  status: %s
status:
  conditions:
  - message: running...
    reason: Running
    status: Unknown
    type: Succeeded
  startTime: "2022-01-01T00:00:00Z"
  taskRuns:
    test-pipeline-fails-to-cancelhello-world-1:
      pipelineTaskName: hello-world-1
`, tc.specStatus))}
			ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
  - name: hello-world-2
    taskRef:
      name: hello-world
`)}
			tasks := []*v1beta1.Task{simpleHelloWorldTask}
			taskRuns := []*v1beta1.TaskRun{
				getTaskRun(
					t,
					"test-pipeline-fails-to-cancelhello-world-1",
					prName,
					"test-pipeline",
					"hello-world",
					corev1.ConditionUnknown,
				),
			}

			cms := []*corev1.ConfigMap{withEnabledAlphaAPIFields(newFeatureFlagsConfigMap())}

			d := test.Data{
				PipelineRuns: prs,
				Pipelines:    ps,
				Tasks:        tasks,
				TaskRuns:     taskRuns,
				ConfigMaps:   cms,
			}

			testAssets, cancel := getPipelineRunController(t, d)
			defer cancel()
			c := testAssets.Controller
			clients := testAssets.Clients
			failingReactorActivated := true

			// Make the patch call fail, i.e. make it so that the controller fails to cancel the TaskRun
			clients.Pipeline.PrependReactor("patch", "taskruns", func(action ktesting.Action) (bool, runtime.Object, error) {
				return failingReactorActivated, nil, fmt.Errorf("i'm sorry Dave, i'm afraid i can't do that")
			})

			err := c.Reconciler.Reconcile(testAssets.Ctx, "foo/test-pipeline-fails-to-cancel")
			if err == nil {
				t.Errorf("Expected to see error returned from reconcile after failing to cancel TaskRun but saw none!")
			}

			// Check that the PipelineRun is still running with correct error message
			reconciledRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(testAssets.Ctx, "test-pipeline-fails-to-cancel", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Somehow had error getting reconciled run out of fake client: %s", err)
			}

			if val, ok := reconciledRun.GetLabels()[pipeline.PipelineLabelKey]; !ok {
				t.Fatalf("expected pipeline label")
			} else if d := cmp.Diff("test-pipeline", val); d != "" {
				t.Errorf("expected to see pipeline label. Diff %s", diff.PrintWantGot(d))
			}

			// The PipelineRun should not be cancelled b/c we couldn't cancel the TaskRun
			checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, ReasonCouldntCancel)
			// The event here is "Normal" because in case we fail to cancel we leave the condition to unknown
			// Further reconcile might converge then the status of the pipeline.
			// See https://github.com/tektoncd/pipeline/issues/2647 for further details.
			wantEvents := []string{
				"Normal PipelineRunCouldntCancel PipelineRun \"test-pipeline-fails-to-cancel\" was cancelled but had errors trying to cancel TaskRuns",
				"Warning InternalError 1 error occurred",
			}
			err = eventstest.CheckEventsOrdered(t, testAssets.Recorder.Events, prName, wantEvents)
			if err != nil {
				t.Errorf(err.Error())
			}

			// Turn off failing reactor and retry reconciliation
			failingReactorActivated = false

			err = c.Reconciler.Reconcile(testAssets.Ctx, "foo/test-pipeline-fails-to-cancel")
			if err == nil {
				// No error is ok
			} else if ok, _ := controller.IsRequeueKey(err); !ok { // Requeue is also fine.
				t.Errorf("Expected to cancel TaskRun successfully!")
			}
		})
	}
}

func TestReconcileFailsTaskRunTimeOut(t *testing.T) {
	prName := "test-pipeline-fails-to-timeout"

	// TestReconcileFailsTaskRunTimeOut runs "Reconcile" on a PipelineRun with a single TaskRun.
	// The TaskRun cannot be timed out. Check that the pipelinerun timeout fails, that reconcile fails and
	// an event is generated
	names.TestingSeed()
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-fails-to-timeout
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  timeout: 1h0m0s
status:
  conditions:
  - message: running...
    reason: Running
    status: Unknown
    type: Succeeded
  startTime: "2021-12-31T22:59:00Z"
  taskRuns:
    test-pipeline-fails-to-timeouthello-world-1:
      pipelineTaskName: hello-world-1
`)}
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
  - name: hello-world-2
    taskRef:
      name: hello-world
`)}
	tasks := []*v1beta1.Task{simpleHelloWorldTask}
	taskRuns := []*v1beta1.TaskRun{
		getTaskRun(
			t,
			"test-pipeline-fails-to-timeouthello-world-1",
			prName,
			"test-pipeline",
			"hello-world",
			corev1.ConditionUnknown,
		),
	}

	cms := []*corev1.ConfigMap{withEnabledAlphaAPIFields(newFeatureFlagsConfigMap())}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        tasks,
		TaskRuns:     taskRuns,
		ConfigMaps:   cms,
	}

	testAssets, cancel := getPipelineRunController(t, d)
	defer cancel()
	c := testAssets.Controller
	clients := testAssets.Clients
	failingReactorActivated := true

	// Make the patch call fail, i.e. make it so that the controller fails to cancel the TaskRun
	clients.Pipeline.PrependReactor("patch", "taskruns", func(action ktesting.Action) (bool, runtime.Object, error) {
		return failingReactorActivated, nil, fmt.Errorf("i'm sorry Dave, i'm afraid i can't do that")
	})

	err := c.Reconciler.Reconcile(testAssets.Ctx, "foo/test-pipeline-fails-to-timeout")
	if err == nil {
		t.Errorf("Expected to see error returned from reconcile after failing to timeout TaskRun but saw none!")
	}

	// Check that the PipelineRun is still running with correct error message
	reconciledRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(testAssets.Ctx, "test-pipeline-fails-to-timeout", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Somehow had error getting reconciled run out of fake client: %s", err)
	}

	if val, ok := reconciledRun.GetLabels()[pipeline.PipelineLabelKey]; !ok {
		t.Fatalf("expected pipeline label")
	} else if d := cmp.Diff("test-pipeline", val); d != "" {
		t.Errorf("expected to see pipeline label. Diff %s", diff.PrintWantGot(d))
	}

	// The PipelineRun should not be timed out b/c we couldn't timeout the TaskRun
	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, ReasonCouldntTimeOut)
	// The event here is "Normal" because in case we fail to timeout we leave the condition to unknown
	// Further reconcile might converge then the status of the pipeline.
	// See https://github.com/tektoncd/pipeline/issues/2647 for further details.
	wantEvents := []string{
		"Normal PipelineRunCouldntTimeOut PipelineRun \"test-pipeline-fails-to-timeout\" was timed out but had errors trying to time out TaskRuns and/or Runs",
		"Warning InternalError 1 error occurred",
	}
	err = eventstest.CheckEventsOrdered(t, testAssets.Recorder.Events, prName, wantEvents)
	if err != nil {
		t.Errorf(err.Error())
	}

	// Turn off failing reactor and retry reconciliation
	failingReactorActivated = false

	err = c.Reconciler.Reconcile(testAssets.Ctx, "foo/test-pipeline-fails-to-timeout")
	if err == nil {
		// No error is ok
	} else if ok, _ := controller.IsRequeueKey(err); !ok { // Requeue is also fine.
		t.Errorf("Expected to timeout TaskRun successfully!")
	}
}

func TestReconcilePropagateLabelsAndAnnotations(t *testing.T) {
	names.TestingSeed()

	ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  annotations:
    PipelineRunAnnotation: PipelineRunValue
  labels:
    PipelineRunLabel: PipelineRunValue
    tekton.dev/pipeline: WillNotBeUsed
  name: test-pipeline-run-with-labels
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	expectedObjectMeta := taskRunObjectMeta("test-pipeline-run-with-labels-hello-world-1", "foo", "test-pipeline-run-with-labels",
		"test-pipeline", "hello-world-1", false)
	expectedObjectMeta.Labels["PipelineRunLabel"] = "PipelineRunValue"
	expectedObjectMeta.Annotations["PipelineRunAnnotation"] = "PipelineRunValue"
	expected := mustParseTaskRunWithObjectMeta(t, expectedObjectMeta, `
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
    kind: Task
`)

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", "test-pipeline-run-with-labels", []string{}, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	// We're ignoring TypeMeta here because parse.MustParseTaskRun populates that, but ktesting does not, so actual does not have it.
	if d := cmp.Diff(expected, actual, cmpopts.IgnoreTypes(metav1.TypeMeta{})); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expected, diff.PrintWantGot(d))
	}
}

func TestReconcilePropagateLabelsWithSpecStatus(t *testing.T) {
	testCases := []struct {
		name       string
		specStatus v1beta1.PipelineRunSpecStatus
	}{
		{
			name:       "pending",
			specStatus: v1beta1.PipelineRunSpecStatusPending,
		}, {
			name:       "cancelled",
			specStatus: v1beta1.PipelineRunSpecStatusCancelled,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			names.TestingSeed()

			ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
			prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  annotations:
    PipelineRunAnnotation: PipelineRunValue
  labels:
    PipelineRunLabel: PipelineRunValue
    tekton.dev/pipeline: WillNotBeUsed
  name: test-pipeline-run-with-labels
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  status: %s
`, tc.specStatus))}

			ts := []*v1beta1.Task{simpleHelloWorldTask}

			d := test.Data{
				PipelineRuns: prs,
				Pipelines:    ps,
				Tasks:        ts,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			_, clients := prt.reconcileRun("foo", "test-pipeline-run-with-labels", []string{}, false)

			reconciledRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(prt.TestAssets.Ctx, "test-pipeline-run-with-labels", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("unexpected error when updating status: %v", err)
			}

			want := "test-pipeline"
			got := reconciledRun.ObjectMeta.Labels["tekton.dev/pipeline"]
			if d := cmp.Diff(want, got); d != "" {
				t.Errorf("expected to see label %v created. Diff %s", want, diff.PrintWantGot(d))
			}
		})
	}
}

func TestReconcileWithDifferentServiceAccounts(t *testing.T) {
	names.TestingSeed()

	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-0
    taskRef:
      name: hello-world-task
  - name: hello-world-1
    taskRef:
      name: hello-world-task
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
  taskRunSpecs:
  - taskServiceAccountName: test-sa-1
    pipelineTaskName: hello-world-1
`)}
	ts := []*v1beta1.Task{parse.MustParseTask(t, `
metadata:
  name: hello-world-task
  namespace: foo
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", []string{}, false)

	taskRunNames := []string{"test-pipeline-run-different-service-accs-hello-world-0", "test-pipeline-run-different-service-accs-hello-world-1"}

	expectedTaskRuns := []*v1beta1.TaskRun{
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta(taskRunNames[0], "foo", "test-pipeline-run-different-service-accs", "test-pipeline", "hello-world-0", false),
			`
spec:
  resources: {}
  serviceAccountName: test-sa-0
  taskRef:
    name: hello-world-task
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta(taskRunNames[1], "foo", "test-pipeline-run-different-service-accs", "test-pipeline", "hello-world-1", false),
			`
spec:
  resources: {}
  serviceAccountName: test-sa-1
  taskRef:
    name: hello-world-task
    kind: Task
`),
	}

	for i := range ps[0].Spec.Tasks {
		// Check that the expected TaskRun was created
		actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").Get(prt.TestAssets.Ctx, taskRunNames[i], metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Expected a TaskRun to be created, but it wasn't: %s", err)
		}
		if d := cmp.Diff(actual, expectedTaskRuns[i], ignoreResourceVersion, ignoreTypeMeta); d != "" {
			t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRuns[i], diff.PrintWantGot(d))
		}

	}
}

func TestReconcileCustomTasksWithDifferentServiceAccounts(t *testing.T) {
	names.TestingSeed()

	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-0
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
  - name: hello-world-1
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
  taskRunSpecs:
  - taskServiceAccountName: test-sa-1
    pipelineTaskName: hello-world-1
`)}

	cms := []*corev1.ConfigMap{withCustomTasks(newFeatureFlagsConfigMap())}
	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", []string{}, false)

	runNames := []string{"test-pipeline-run-different-service-accs-hello-world-0", "test-pipeline-run-different-service-accs-hello-world-1"}
	expectedSANames := []string{"test-sa-0", "test-sa-1"}

	for i := range ps[0].Spec.Tasks {
		actual, err := clients.Pipeline.TektonV1alpha1().Runs("foo").Get(prt.TestAssets.Ctx, runNames[i], metav1.GetOptions{})
		if err != nil {
			t.Errorf("Expected a Run %s to be created but it wasn't: %s", runNames[i], err)
			continue
		}
		if actual.Spec.ServiceAccountName != expectedSANames[i] {
			t.Errorf("Expected Run %s to have service account %s but it was %s", runNames[i], expectedSANames[i], actual.Spec.ServiceAccountName)
		}
	}
}

// TestReconcileWithTimeoutAndRetry runs "Reconcile" against pipelines with
// retries and timeout settings, and status that represents different number of
// retries already performed.  It verifies the reconciled status and events
// generated
func TestReconcileWithTimeoutAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name               string
		retries            int
		conditionSucceeded corev1.ConditionStatus
		wantEvents         []string
	}{{
		name:               "One try has to be done",
		retries:            1,
		conditionSucceeded: corev1.ConditionFalse,
		wantEvents: []string{
			"Warning Failed PipelineRun \"test-pipeline-retry-run-with-timeout\" failed to finish within",
		},
	}, {
		name:               "No more retries are needed",
		retries:            2,
		conditionSucceeded: corev1.ConditionUnknown,
		wantEvents: []string{
			"Warning Failed PipelineRun \"test-pipeline-retry-run-with-timeout\" failed to finish within",
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: test-pipeline-retry
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    retries: %d
    taskRef:
      name: hello-world
`, tc.retries))}
			prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-retry-run-with-timeout
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline-retry
  serviceAccountName: test-sa
  timeout: 12h0m0s
status:
  startTime: "2021-12-31T00:00:00Z"
`)}

			ts := []*v1beta1.Task{
				simpleHelloWorldTask,
			}
			trs := []*v1beta1.TaskRun{parse.MustParseTaskRun(t, `
metadata:
  name: hello-world-1
  namespace: foo
status:
  conditions:
  - status: "False"
    type: Succeeded
  podName: my-pod-name
  retriesStatus:
  - conditions:
    - status: "False"
      type: Succeeded
`)}

			prtrs := &v1beta1.PipelineRunTaskRunStatus{
				PipelineTaskName: "hello-world-1",
				Status:           &trs[0].Status,
			}
			prs[0].Status.TaskRuns = make(map[string]*v1beta1.PipelineRunTaskRunStatus)
			prs[0].Status.TaskRuns["hello-world-1"] = prtrs

			d := test.Data{
				PipelineRuns: prs,
				Pipelines:    ps,
				Tasks:        ts,
				TaskRuns:     trs,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-retry-run-with-timeout", []string{}, false)

			if len(reconciledRun.Status.TaskRuns["hello-world-1"].Status.RetriesStatus) != tc.retries {
				t.Fatalf(" %d retries expected but got %d ", tc.retries, len(reconciledRun.Status.TaskRuns["hello-world-1"].Status.RetriesStatus))
			}

			if status := reconciledRun.Status.TaskRuns["hello-world-1"].Status.GetCondition(apis.ConditionSucceeded).Status; status != tc.conditionSucceeded {
				t.Fatalf("Succeeded expected to be %s but is %s", tc.conditionSucceeded, status)
			}
		})
	}
}

// TestReconcileAndPropagateCustomPipelineTaskRunSpec tests that custom PipelineTaskRunSpec declared
// in PipelineRun is propagated to created TaskRuns
func TestReconcileAndPropagateCustomPipelineTaskRunSpec(t *testing.T) {
	names.TestingSeed()
	prName := "test-pipeline-run"
	ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  annotations:
    PipelineRunAnnotation: PipelineRunValue
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  taskRunSpecs:
  - pipelineTaskName: hello-world-1
    sidecarOverrides:
    - name: bar
    stepOverrides:
    - name: foo
    taskPodTemplate:
      nodeSelector:
        workloadtype: tekton
    taskServiceAccountName: custom-sa
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", prName, []string{}, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRunObjectMeta := taskRunObjectMeta("test-pipeline-run-hello-world-1", "foo", "test-pipeline-run", "test-pipeline", "hello-world-1", false)
	expectedTaskRunObjectMeta.Annotations["PipelineRunAnnotation"] = "PipelineRunValue"
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t, expectedTaskRunObjectMeta, `
spec:
  podTemplate:
    nodeSelector:
      workloadtype: tekton
  resources: {}
  serviceAccountName: custom-sa
  sidecarOverrides:
  - name: bar
  stepOverrides:
  - name: foo
  taskRef:
    name: hello-world
    kind: Task
`)

	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see propagated custom ServiceAccountName and PodTemplate in TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}
}

func TestReconcileCustomTasksWithTaskRunSpec(t *testing.T) {
	names.TestingSeed()
	prName := "test-pipeline-run"
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
`)}

	serviceAccount := "custom-sa"
	podTemplate := &pod.Template{
		NodeSelector: map[string]string{
			"workloadtype": "tekton",
		},
	}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  taskRunSpecs:
  - pipelineTaskName: hello-world-1
    taskPodTemplate:
      nodeSelector:
        workloadtype: tekton
    taskServiceAccountName: custom-sa
`)}

	cms := []*corev1.ConfigMap{withCustomTasks(newFeatureFlagsConfigMap())}
	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", prName, []string{}, false)

	runName := "test-pipeline-run-hello-world-1"

	actual, err := clients.Pipeline.TektonV1alpha1().Runs("foo").Get(prt.TestAssets.Ctx, runName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected a Run %s to be created but it wasn't: %s", runName, err)
	}
	if actual.Spec.ServiceAccountName != serviceAccount {
		t.Errorf("Expected Run %s to have service account %s but it was %s", runName, serviceAccount, actual.Spec.ServiceAccountName)
	}
	if d := cmp.Diff(actual.Spec.PodTemplate, podTemplate); d != "" {
		t.Errorf("Incorrect pod template in Run %s. Diff %s", runName, diff.PrintWantGot(d))
	}

}

func ensurePVCCreated(ctx context.Context, t *testing.T, clients test.Clients, name, namespace string) {
	t.Helper()
	_, err := clients.Kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Expected PVC %s to be created for VolumeResource but did not exist", name)
	}
	pvcCreated := false
	for _, a := range clients.Kube.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "persistentvolumeclaims" {
			pvcCreated = true
		}
	}
	if !pvcCreated {
		t.Errorf("Expected to see volume resource PVC created but didn't")
	}
}

func TestReconcileWithWhenExpressionsWithTaskResultsAndParams(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileWithWhenExpressionsWithTaskResultsAndParams(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileWithWhenExpressionsWithTaskResultsAndParams(t *testing.T, embeddedStatus string) {
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  params:
  - name: run
    type: string
  tasks:
  - name: a-task
    taskRef:
      name: a-task
  - name: b-task
    taskRef:
      name: b-task
    when:
    - input: $(tasks.a-task.results.aResult)
      operator: in
      values:
      - aResultValue
    - input: aResultValue
      operator: in
      values:
      - $(tasks.a-task.results.aResult)
    - input: $(params.run)
      operator: in
      values:
      - "yes"
  - name: c-task
    taskRef:
      name: c-task
    when:
    - input: $(tasks.a-task.results.aResult)
      operator: in
      values:
      - missing
    - input: $(params.run)
      operator: notin
      values:
      - "yes"
  - name: d-task
    runAfter:
    - c-task
    taskRef:
      name: d-task
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  params:
  - name: run
    value: "yes"
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
`)}
	ts := []*v1beta1.Task{
		{ObjectMeta: baseObjectMeta("a-task", "foo")},
		{ObjectMeta: baseObjectMeta("b-task", "foo")},
		{ObjectMeta: baseObjectMeta("c-task", "foo")},
		{ObjectMeta: baseObjectMeta("d-task", "foo")},
	}
	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-different-service-accs-a-task-xxyyy", "foo", "test-pipeline-run-different-service-accs",
			"test-pipeline", "a-task", true),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
  timeout: 1h0m0s
status:
  conditions:
  - status: "True"
    type: Succeeded
  taskResults:
  - name: aResult
    value: aResultValue
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
		ConfigMaps:   []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 1 \\(Failed: 0, Cancelled 0\\), Incomplete: 2, Skipped: 1",
	}
	pipelineRun, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", wantEvents, false)

	expectedTaskRunName := "test-pipeline-run-different-service-accs-b-task"
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta(expectedTaskRunName, "foo", "test-pipeline-run-different-service-accs", "test-pipeline", "b-task", false),
		`
spec:
  resources: {}
  serviceAccountName: test-sa-0
  taskRef:
    name: b-task
    kind: Task
`)
	// Check that the expected TaskRun was created
	actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
		LabelSelector: "tekton.dev/pipelineTask=b-task,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs",
		Limit:         1,
	})

	if err != nil {
		t.Fatalf("Failure to list TaskRun's %s", err)
	}
	if len(actual.Items) != 1 {
		t.Fatalf("Expected 1 TaskRuns got %d", len(actual.Items))
	}
	actualTaskRun := actual.Items[0]
	if d := cmp.Diff(expectedTaskRun, &actualTaskRun, ignoreResourceVersion, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRunName, diff.PrintWantGot(d))
	}

	expectedWhenExpressionsInTaskRun := []v1beta1.WhenExpression{{
		Input:    "aResultValue",
		Operator: "in",
		Values:   []string{"aResultValue"},
	}, {
		Input:    "aResultValue",
		Operator: "in",
		Values:   []string{"aResultValue"},
	}, {
		Input:    "yes",
		Operator: "in",
		Values:   []string{"yes"},
	}}
	verifyTaskRunStatusesWhenExpressions(t, embeddedStatus, pipelineRun.Status, expectedTaskRunName, expectedWhenExpressionsInTaskRun)

	actualSkippedTasks := pipelineRun.Status.SkippedTasks
	expectedSkippedTasks := []v1beta1.SkippedTask{{
		Name:   "c-task",
		Reason: v1beta1.WhenExpressionsSkip,
		WhenExpressions: v1beta1.WhenExpressions{{
			Input:    "aResultValue",
			Operator: "in",
			Values:   []string{"missing"},
		}, {
			Input:    "yes",
			Operator: "notin",
			Values:   []string{"yes"},
		}},
	}}
	if d := cmp.Diff(actualSkippedTasks, expectedSkippedTasks); d != "" {
		t.Errorf("expected to find Skipped Tasks %v. Diff %s", expectedSkippedTasks, diff.PrintWantGot(d))
	}

	skippedTasks := []string{"c-task"}
	for _, skippedTask := range skippedTasks {
		labelSelector := fmt.Sprintf("tekton.dev/pipelineTask=%s,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs", skippedTask)
		actualSkippedTask, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
			Limit:         1,
		})
		if err != nil {
			t.Fatalf("Failure to list TaskRun's %s", err)
		}
		if len(actualSkippedTask.Items) != 0 {
			t.Fatalf("Expected 0 TaskRuns got %d", len(actualSkippedTask.Items))
		}
	}
}

func TestReconcileWithWhenExpressions(t *testing.T) {
	//		(b)
	//		/
	//	(a) ———— (c) ———— (d)
	//		\
	//		(e) ———— (f)
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
# a-task is skipped because its when expressions evaluate to false
  - name: a-task
    taskRef:
      name: a-task
    when:
    - input: foo
      operator: in
      values:
      - bar
# b-task is executed regardless of running after skipped a-task because when expressions are scoped to task
  - name: b-task
    runAfter:
    - a-task
    taskRef:
      name: b-task
# c-task is skipped because its when expressions evaluate to false (not because it's parent a-task is skipped)
  - name: c-task
    runAfter:
    - a-task
    taskRef:
      name: c-task
    when:
    - input: foo
      operator: in
      values:
      - bar
# d-task is executed regardless of running after skipped parent c-task (and skipped grandparent a-task)
# because when expressions are scoped to task
  - name: d-task
    runAfter:
    - c-task
    taskRef:
      name: d-task
# e-task is attempted regardless of running after skipped a-task because when expressions are scoped to task
# but then get skipped because of missing result references from a-task
  - name: e-task
    taskRef:
      name: e-task
    when:
    - input: $(tasks.a-task.results.aResult)
      operator: in
      values:
      - aResultValue
# f-task is skipped because its parent task e-task is skipped because of missing result reference from a-task
  - name: f-task
    runAfter:
    - e-task
    taskRef:
      name: f-task
`)}
	// initialize the pipelinerun with the skipped a-task
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
status:
  skippedTasks:
  - name: a-task
    whenExpressions:
    - input: foo
      operator: in
      values:
      - bar
`)}
	// initialize the tasks used in the pipeline
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task
  namespace: foo
spec:
  results:
  - description: a result
    name: aResult
`),
		{ObjectMeta: baseObjectMeta("b-task", "foo")},
		{ObjectMeta: baseObjectMeta("c-task", "foo")},
		{ObjectMeta: baseObjectMeta("d-task", "foo")},
		{ObjectMeta: baseObjectMeta("e-task", "foo")},
		{ObjectMeta: baseObjectMeta("f-task", "foo")},
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0 \\(Failed: 0, Cancelled 0\\), Incomplete: 2, Skipped: 4",
	}
	pipelineRun, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", wantEvents, false)

	taskRunExists := func(taskName string, taskRunName string) {
		expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta(taskRunName, "foo", "test-pipeline-run-different-service-accs",
				"test-pipeline", taskName, false),
			fmt.Sprintf(`
spec:
  resources: {}
  serviceAccountName: test-sa-0
  taskRef:
    name: %s
    kind: Task
`, taskName))

		actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("tekton.dev/pipelineTask=%s,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs", taskName),
			Limit:         1,
		})

		if err != nil {
			t.Fatalf("Failure to list TaskRuns %s", err)
		}
		if len(actual.Items) != 1 {
			t.Fatalf("Expected 1 TaskRun got %d", len(actual.Items))
		}
		actualTaskRun := actual.Items[0]
		if d := cmp.Diff(expectedTaskRun, &actualTaskRun, ignoreResourceVersion, ignoreTypeMeta); d != "" {
			t.Errorf("expected to see TaskRun %v created. Diff %s", taskRunName, diff.PrintWantGot(d))
		}
	}

	taskRunExists("b-task", "test-pipeline-run-different-service-accs-b-task")
	taskRunExists("d-task", "test-pipeline-run-different-service-accs-d-task")

	actualSkippedTasks := pipelineRun.Status.SkippedTasks
	expectedSkippedTasks := []v1beta1.SkippedTask{{
		// its when expressions evaluate to false
		Name:   "a-task",
		Reason: v1beta1.WhenExpressionsSkip,
		WhenExpressions: v1beta1.WhenExpressions{{
			Input:    "foo",
			Operator: "in",
			Values:   []string{"bar"},
		}},
	}, {
		// its when expressions evaluate to false
		Name:   "c-task",
		Reason: v1beta1.WhenExpressionsSkip,
		WhenExpressions: v1beta1.WhenExpressions{{
			Input:    "foo",
			Operator: "in",
			Values:   []string{"bar"},
		}},
	}, {
		// was attempted, but has missing results references
		Name:   "e-task",
		Reason: v1beta1.MissingResultsSkip,
		WhenExpressions: v1beta1.WhenExpressions{{
			Input:    "$(tasks.a-task.results.aResult)",
			Operator: "in",
			Values:   []string{"aResultValue"},
		}},
	}, {
		Name:   "f-task",
		Reason: v1beta1.ParentTasksSkip,
	}}
	if d := cmp.Diff(expectedSkippedTasks, actualSkippedTasks); d != "" {
		t.Errorf("expected to find Skipped Tasks %v. Diff %s", expectedSkippedTasks, diff.PrintWantGot(d))
	}

	// confirm that there are no taskruns created for the skipped tasks
	skippedTasks := []string{"a-task", "c-task", "e-task", "f-task"}
	for _, skippedTask := range skippedTasks {
		labelSelector := fmt.Sprintf("tekton.dev/pipelineTask=%s,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs", skippedTask)
		actualSkippedTask, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
			Limit:         1,
		})
		if err != nil {
			t.Fatalf("Failure to list TaskRun's %s", err)
		}
		if len(actualSkippedTask.Items) != 0 {
			t.Fatalf("Expected 0 TaskRuns got %d", len(actualSkippedTask.Items))
		}
	}
}

func TestReconcileWithWhenExpressionsWithResultRefs(t *testing.T) {
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
# a-task is executed and produces a result aResult with value aResultValue
  - name: a-task
    taskRef:
      name: a-task
# b-task is skipped because it has when expressions, with result reference to a-task, that evaluate to false
  - name: b-task
    taskRef:
      name: b-task
    when:
    - input: $(tasks.a-task.results.aResult)
      operator: in
      values:
      - notResultValue
# c-task is executed regardless of running after skipped b-task because when expressions are scoped to task
  - name: c-task
    runAfter:
    - b-task
    taskRef:
      name: c-task
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task
  namespace: foo
spec:
  results:
  - description: a result
    name: aResult
`),
		{ObjectMeta: baseObjectMeta("b-task", "foo")},
		{ObjectMeta: baseObjectMeta("c-task", "foo")},
	}
	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-different-service-accs-a-task-xxyyy", "foo",
			"test-pipeline-run-different-service-accs", "test-pipeline", "a-task",
			true),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
  timeout: 1h0m0s
status:
  conditions:
  - status: "True"
    type: Succeeded
  taskResults:
  - name: aResult
    value: aResultValue
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 1 \\(Failed: 0, Cancelled 0\\), Incomplete: 1, Skipped: 1",
	}
	pipelineRun, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", wantEvents, false)

	actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
		LabelSelector: "tekton.dev/pipelineTask=c-task,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs",
		Limit:         1,
	})

	if err != nil {
		t.Fatalf("Failure to list TaskRuns %s", err)
	}
	if len(actual.Items) != 1 {
		t.Fatalf("Expected 1 TaskRun got %d", len(actual.Items))
	}

	actualSkippedTasks := pipelineRun.Status.SkippedTasks
	expectedSkippedTasks := []v1beta1.SkippedTask{{
		// its when expressions evaluate to false
		Name:   "b-task",
		Reason: v1beta1.WhenExpressionsSkip,
		WhenExpressions: v1beta1.WhenExpressions{{
			Input:    "aResultValue",
			Operator: "in",
			Values:   []string{"notResultValue"},
		}},
	}}
	if d := cmp.Diff(expectedSkippedTasks, actualSkippedTasks); d != "" {
		t.Errorf("expected to find Skipped Tasks %v. Diff %s", expectedSkippedTasks, diff.PrintWantGot(d))
	}

	// confirm that there are no taskruns created for the skipped tasks
	skippedTasks := []string{"b-task"}
	for _, skippedTask := range skippedTasks {
		labelSelector := fmt.Sprintf("tekton.dev/pipelineTask=%s,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs", skippedTask)
		actualSkippedTask, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
			Limit:         1,
		})
		if err != nil {
			t.Fatalf("Failure to list TaskRun's %s", err)
		}
		if len(actualSkippedTask.Items) != 0 {
			t.Fatalf("Expected 0 TaskRuns got %d", len(actualSkippedTask.Items))
		}
	}
}

// TestReconcileWithAffinityAssistantStatefulSet tests that given a pipelineRun with workspaces,
// an Affinity Assistant StatefulSet is created for each PVC workspace and
// that the Affinity Assistant names is propagated to TaskRuns.
func TestReconcileWithAffinityAssistantStatefulSet(t *testing.T) {
	workspaceName := "ws1"
	workspaceName2 := "ws2"
	pipelineRunName := "test-pipeline-run"
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      workspace: ws1
  - name: hello-world-2
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      workspace: ws2
  - name: hello-world-3
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      workspace: emptyDirWorkspace
  workspaces:
  - name: ws1
  - name: ws2
  - name: emptyDirWorkspace
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  workspaces:
  - name: ws1
    volumeClaimTemplate:
      metadata:
        creationTimestamp: null
        name: myclaim
  - name: ws2
    volumeClaimTemplate:
      metadata:
        creationTimestamp: null
        name: myclaim2
  - emptyDir: {}
    name: emptyDirWorkspace
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", pipelineRunName, []string{}, false)

	// Check that the expected StatefulSet was created
	stsNames := make([]string, 0)
	for _, a := range clients.Kube.Actions() {
		if ca, ok := a.(ktesting.CreateAction); ok {
			obj := ca.GetObject()
			if sts, ok := obj.(*appsv1.StatefulSet); ok {
				stsNames = append(stsNames, sts.Name)
			}
		}
	}

	if len(stsNames) != 2 {
		t.Fatalf("expected one StatefulSet created. %d was created", len(stsNames))
	}

	expectedAffinityAssistantName1 := getAffinityAssistantName(workspaceName, pipelineRunName)
	expectedAffinityAssistantName2 := getAffinityAssistantName(workspaceName2, pipelineRunName)
	expectedAffinityAssistantStsNames := make(map[string]bool)
	expectedAffinityAssistantStsNames[expectedAffinityAssistantName1] = true
	expectedAffinityAssistantStsNames[expectedAffinityAssistantName2] = true
	for _, stsName := range stsNames {
		_, found := expectedAffinityAssistantStsNames[stsName]
		if !found {
			t.Errorf("unexpected StatefulSet created, named %s", stsName)
		}
	}

	taskRuns, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error when listing TaskRuns: %v", err)
	}

	if len(taskRuns.Items) != 3 {
		t.Errorf("expected two TaskRuns created. %d was created", len(taskRuns.Items))
	}

	taskRunsWithPropagatedAffinityAssistantName := 0
	for _, tr := range taskRuns.Items {
		for _, ws := range tr.Spec.Workspaces {
			propagatedAffinityAssistantName := tr.Annotations["pipeline.tekton.dev/affinity-assistant"]
			if ws.PersistentVolumeClaim != nil {

				if propagatedAffinityAssistantName != expectedAffinityAssistantName1 && propagatedAffinityAssistantName != expectedAffinityAssistantName2 {
					t.Fatalf("found taskRun with PVC workspace, but with unexpected AffinityAssistantAnnotation value; expected %s or %s, got %s", expectedAffinityAssistantName1, expectedAffinityAssistantName2, propagatedAffinityAssistantName)
				}
				taskRunsWithPropagatedAffinityAssistantName++
			}

			if ws.PersistentVolumeClaim == nil {
				if propagatedAffinityAssistantName != "" {
					t.Fatalf("found taskRun workspace that is not PVC workspace, but with unexpected AffinityAssistantAnnotation; expected NO AffinityAssistantAnnotation, got %s", propagatedAffinityAssistantName)
				}
			}
		}
	}

	if taskRunsWithPropagatedAffinityAssistantName != 2 {
		t.Errorf("expected only one of two TaskRuns to have Affinity Assistant affinity. %d was detected", taskRunsWithPropagatedAffinityAssistantName)
	}

	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun to be running, but condition status is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}
}

// TestReconcileWithVolumeClaimTemplateWorkspace tests that given a pipeline with volumeClaimTemplate workspace,
// a PVC is created and that the workspace appears as a PersistentVolumeClaim workspace for TaskRuns.
func TestReconcileWithVolumeClaimTemplateWorkspace(t *testing.T) {
	pipelineRunName := "test-pipeline-run"
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      workspace: ws1
  - name: hello-world-2
    taskRef:
      name: hello-world
  workspaces:
  - name: ws1
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  workspaces:
  - name: ws1
    volumeClaimTemplate:
      metadata:
        creationTimestamp: null
        name: myclaim
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", pipelineRunName, []string{}, false)

	// Check that the expected PVC was created
	pvcNames := make([]string, 0)
	for _, a := range clients.Kube.Actions() {
		if ca, ok := a.(ktesting.CreateAction); ok {
			obj := ca.GetObject()
			if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				pvcNames = append(pvcNames, pvc.Name)
			}
		}
	}

	if len(pvcNames) != 1 {
		t.Errorf("expected one PVC created. %d was created", len(pvcNames))
	}

	for _, pr := range prs {
		for _, w := range pr.Spec.Workspaces {
			expectedPVCName := volumeclaim.GetPersistentVolumeClaimName(&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: w.VolumeClaimTemplate.Name,
				},
			}, w, *kmeta.NewControllerRef(pr))
			_, err := clients.Kube.CoreV1().PersistentVolumeClaims(pr.Namespace).Get(prt.TestAssets.Ctx, expectedPVCName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("expected PVC %s to exist but instead got error when getting it: %v", expectedPVCName, err)
			}
		}
	}

	taskRuns, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error when listing TaskRuns: %v", err)
	}

	for _, tr := range taskRuns.Items {
		for _, ws := range tr.Spec.Workspaces {
			if ws.VolumeClaimTemplate != nil {
				t.Fatalf("found volumeClaimTemplate workspace. Did not expect to find any taskruns with volumeClaimTemplate workspaces")
			}

			if ws.PersistentVolumeClaim == nil {
				t.Fatalf("found taskRun workspace that is not PersistentVolumeClaim workspace. Did only expect PersistentVolumeClaims workspaces")
			}
		}
	}

	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun to be running, but condition status is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}
}

// TestReconcileWithVolumeClaimTemplateWorkspaceUsingSubPaths tests that given a pipeline with volumeClaimTemplate workspace and
// multiple instances of the same task, but using different subPaths in the volume - is seen as taskRuns with expected subPaths.
func TestReconcileWithVolumeClaimTemplateWorkspaceUsingSubPaths(t *testing.T) {
	subPath1 := "customdirectory"
	subPath2 := "otherdirecory"
	pipelineRunWsSubPath := "mypath"
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      subPath: customdirectory
      workspace: ws1
  - name: hello-world-2
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      subPath: otherdirecory
      workspace: ws1
  - name: hello-world-3
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      workspace: ws1
  - name: hello-world-4
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      workspace: ws2
  - name: hello-world-5
    taskRef:
      name: hello-world
    workspaces:
    - name: taskWorkspaceName
      subPath: customdirectory
      workspace: ws2
  workspaces:
  - name: ws1
  - name: ws2
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  workspaces:
  - name: ws1
    volumeClaimTemplate:
      metadata:
        creationTimestamp: null
        name: myclaim
  - name: ws2
    subPath: mypath
    volumeClaimTemplate:
      metadata:
        creationTimestamp: null
        name: myclaim
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run", []string{}, false)

	taskRuns, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error when listing TaskRuns: %v", err)
	}

	if len(taskRuns.Items) != 5 {
		t.Fatalf("unexpected number of taskRuns found, expected 2, but found %d", len(taskRuns.Items))
	}

	hasSeenWorkspaceWithPipelineTaskSubPath1 := false
	hasSeenWorkspaceWithPipelineTaskSubPath2 := false
	hasSeenWorkspaceWithEmptyPipelineTaskSubPath := false
	hasSeenWorkspaceWithRunSubPathAndEmptyPipelineTaskSubPath := false
	hasSeenWorkspaceWithRunSubPathAndPipelineTaskSubPath1 := false
	for _, tr := range taskRuns.Items {
		for _, ws := range tr.Spec.Workspaces {

			if ws.PersistentVolumeClaim == nil {
				t.Fatalf("found taskRun workspace that is not PersistentVolumeClaim workspace. Did only expect PersistentVolumeClaims workspaces")
			}

			if ws.SubPath == subPath1 {
				hasSeenWorkspaceWithPipelineTaskSubPath1 = true
			}

			if ws.SubPath == subPath2 {
				hasSeenWorkspaceWithPipelineTaskSubPath2 = true
			}

			if ws.SubPath == "" {
				hasSeenWorkspaceWithEmptyPipelineTaskSubPath = true
			}

			if ws.SubPath == pipelineRunWsSubPath {
				hasSeenWorkspaceWithRunSubPathAndEmptyPipelineTaskSubPath = true
			}

			if ws.SubPath == fmt.Sprintf("%s/%s", pipelineRunWsSubPath, subPath1) {
				hasSeenWorkspaceWithRunSubPathAndPipelineTaskSubPath1 = true
			}
		}
	}

	if !hasSeenWorkspaceWithPipelineTaskSubPath1 {
		t.Fatalf("did not see a taskRun with a workspace using pipelineTask subPath1")
	}

	if !hasSeenWorkspaceWithPipelineTaskSubPath2 {
		t.Fatalf("did not see a taskRun with a workspace using pipelineTask subPath2")
	}

	if !hasSeenWorkspaceWithEmptyPipelineTaskSubPath {
		t.Fatalf("did not see a taskRun with a workspace using empty pipelineTask subPath")
	}

	if !hasSeenWorkspaceWithRunSubPathAndEmptyPipelineTaskSubPath {
		t.Fatalf("did not see a taskRun with workspace using empty pipelineTask subPath and a subPath from pipelineRun")
	}

	if !hasSeenWorkspaceWithRunSubPathAndPipelineTaskSubPath1 {
		t.Fatalf("did not see a taskRun with workspace using pipelineTaks subPath1 and a subPath from pipelineRun")
	}

	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun to be running, but condition status is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}
}

func TestReconcileWithTaskResults(t *testing.T) {
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: a-task
    taskRef:
      name: a-task
  - name: b-task
    params:
    - name: bParam
      value: $(tasks.a-task.results.aResult)
    taskRef:
      name: b-task
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task
  namespace: foo
spec: {}
`),
		parse.MustParseTask(t, `
metadata:
  name: b-task
  namespace: foo
spec:
  params:
  - name: bParam
    type: string
`),
	}
	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-different-service-accs-a-task-xxyyy", "foo",
			"test-pipeline-run-different-service-accs", "test-pipeline", "a-task", true),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
  timeout: 1h0m0s
status:
  conditions:
  - lastTransitionTime: null
    status: "True"
    type: Succeeded
  taskResults:
  - name: aResult
    value: aResultValue
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", []string{}, false)

	expectedTaskRunName := "test-pipeline-run-different-service-accs-b-task"
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-different-service-accs-b-task", "foo",
			"test-pipeline-run-different-service-accs", "test-pipeline", "b-task", false),
		`
spec:
  params:
  - name: bParam
    value: aResultValue
  resources: {}
  serviceAccountName: test-sa-0
  taskRef:
    name: b-task
    kind: Task
`)
	// Check that the expected TaskRun was created
	actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
		LabelSelector: "tekton.dev/pipelineTask=b-task,tekton.dev/pipelineRun=test-pipeline-run-different-service-accs",
		Limit:         1,
	})

	if err != nil {
		t.Fatalf("Failure to list TaskRun's %s", err)
	}
	if len(actual.Items) != 1 {
		t.Fatalf("Expected 1 TaskRuns got %d", len(actual.Items))
	}
	actualTaskRun := actual.Items[0]
	if d := cmp.Diff(expectedTaskRun, &actualTaskRun, ignoreResourceVersion, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRunName, diff.PrintWantGot(d))
	}
}

func TestReconcileWithTaskResultsEmbeddedNoneStarted(t *testing.T) {
	names.TestingSeed()
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-different-service-accs
  namespace: foo
spec:
  params:
  - name: foo
    value: bar
  pipelineSpec:
    params:
    - name: foo
      type: string
    tasks:
    - name: a-task
      taskRef:
        name: a-task
    - name: b-task
      params:
      - name: bParam
        value: $(params.foo)/baz@$(tasks.a-task.results.A_RESULT)
      taskRef:
        name: b-task
  serviceAccountName: test-sa-0
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task
  namespace: foo
spec:
  results:
  - name: A_RESULT
`),
		parse.MustParseTask(t, `
metadata:
  name: b-task
  namespace: foo
spec:
  params:
  - name: bParam
    type: string
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-different-service-accs", []string{}, false)

	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun to be running, but condition status is %s", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	// Since b-task is dependent on a-task, via the results, only a-task should run
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-different-service-accs-a-task", "foo",
			"test-pipeline-run-different-service-accs", "test-pipeline-run-different-service-accs", "a-task", false),
		`
spec:
  resources: {}
  serviceAccountName: test-sa-0
  taskRef:
    kind: Task
    name: a-task
`)
	// Check that the expected TaskRun was created (only)
	actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failure to list TaskRun's %s", err)
	}
	if len(actual.Items) != 1 {
		t.Fatalf("Expected 1 TaskRuns got %d", len(actual.Items))
	}
	actualTaskRun := actual.Items[0]
	if d := cmp.Diff(expectedTaskRun, &actualTaskRun, ignoreResourceVersion, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}
}

func TestReconcileWithPipelineResults(t *testing.T) {
	testCases := []struct {
		name        string
		embeddedVal string
	}{
		{
			name:        "default embedded status",
			embeddedVal: config.DefaultEmbeddedStatus,
		},
		{
			name:        "full embedded status",
			embeddedVal: config.FullEmbeddedStatus,
		},
		{
			name:        "both embedded status",
			embeddedVal: config.BothEmbeddedStatus,
		},
		{
			name:        "minimal embedded status",
			embeddedVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileWithPipelineResults(t, tc.embeddedVal)
			runTestReconcileWithFinallyResults(t, tc.embeddedVal)
		})
	}
}

func runTestReconcileWithFinallyResults(t *testing.T, embeddedStatus string) {
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  results:
    - description: pipeline result
      name: result
      value: $(finally.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(finally.b-task.results.b-Result)
  tasks:
    - name: c-task
      taskRef:
        name: c-task
  finally:
    - name: a-task
      taskRef:
        name: a-task
    - name: b-task
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
        name: b-task
`)}
	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-finally-results-task-run-a", "foo",
			"test-pipeline-run-finally-results", "test-pipeline", "a-task", true),
		`
spec:
  taskRef:
    name: hello-world
status:
  conditions:
  - status: "True"
    type: Succeeded
  taskResults:
  - name: a-Result
    value: aResultValue
`), mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-finally-results-task-run-c", "foo",
			"test-pipeline-run-finally-results", "test-pipeline", "c-task", true),
		`
spec:
  taskRef:
    name: hello-world
status:
  conditions:
  - status: "True"
    type: Succeeded
`)}
	rs := []*v1alpha1.Run{mustParseRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-finally-results-task-run-b", "foo",
			"test-pipeline-run-finally-results", "test-pipeline", "b-task", true),
		`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
status:
  conditions:
  - status: "True"
    type: Succeeded
  results:
  - name: b-Result
    value: bResultValue
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-finally-results
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
status:
  conditions:
  - status: "Unknown"
    type: Succeeded
    reason: Succeeded
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task
  namespace: foo
spec:
  results:
  - name: a-Result
`),
		parse.MustParseTask(t, `
metadata:
  name: c-task
  namespace: foo
spec:
  steps:
  - image: ubuntu
    script: |
      #!/usr/bin/env bash
      echo "Hello from bash!"

`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
		Runs:         rs,
		ConfigMaps:   []*corev1.ConfigMap{withCustomTasks(withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus))},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-finally-results", []string{}, false)

	expectedPrFullStatus := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-finally-results
  namespace: foo
  labels:
    tekton.dev/pipeline: test-pipeline
  annotations: {}
spec:
  pipelineRef:
    name: test-pipeline
status:
  pipelineSpec:
    results:
    - description: pipeline result
      name: result
      value: $(finally.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(finally.b-task.results.b-Result)
    tasks:
    - name: c-task
      taskRef:
        name: c-task
        kind: Task
    finally:
    - name: a-task
      taskRef:
        name: a-task
        kind: Task
    - name: b-task
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
        name: b-task
  conditions:
  - status: "True"
    type: Succeeded
    reason: Succeeded
    message: "Tasks Completed: 3 (Failed: 0, Cancelled 0), Skipped: 0"
  pipelineResults:
  - name: result
    value: aResultValue
  - name: custom-result
    value: bResultValue
  taskRuns:
    test-pipeline-run-finally-results-task-run-a:
      pipelineTaskName: a-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        taskResults:
        - name: a-Result
          value: aResultValue
    test-pipeline-run-finally-results-task-run-c:
      pipelineTaskName: c-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
  runs:
    test-pipeline-run-finally-results-task-run-b:
      pipelineTaskName: b-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        results:
        - name: b-Result
          value: bResultValue
`)

	expectedPrBothStatus := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-finally-results
  namespace: foo
  labels:
    tekton.dev/pipeline: test-pipeline
  annotations: {}
spec:
  pipelineRef:
    name: test-pipeline
status:
  pipelineSpec:
    results:
    - description: pipeline result
      name: result
      value: $(finally.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(finally.b-task.results.b-Result)
    tasks:
    - name: c-task
      taskRef:
        name: c-task
        kind: Task
    finally:
    - name: a-task
      taskRef:
        name: a-task
        kind: Task
    - name: b-task
      taskRef:
        name: b-task
        apiVersion: example.dev/v0
        kind: Example
  conditions:
  - status: "True"
    type: Succeeded
    reason: Succeeded
    message: "Tasks Completed: 3 (Failed: 0, Cancelled 0), Skipped: 0"
  pipelineResults:
  - name: result
    value: aResultValue
  - name: custom-result
    value: bResultValue
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: test-pipeline-run-finally-results-task-run-c
    pipelineTaskName: c-task
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: test-pipeline-run-finally-results-task-run-a
    pipelineTaskName: a-task
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: test-pipeline-run-finally-results-task-run-b
    pipelineTaskName: b-task
  taskRuns:
    test-pipeline-run-finally-results-task-run-a:
      pipelineTaskName: a-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        taskResults:
        - name: a-Result
          value: aResultValue
    test-pipeline-run-finally-results-task-run-c:
      pipelineTaskName: c-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
  runs:
    test-pipeline-run-finally-results-task-run-b:
      pipelineTaskName: b-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        results:
        - name: b-Result
          value: bResultValue
`)

	expectedPrMinimalStatus := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-finally-results
  namespace: foo
  labels:
    tekton.dev/pipeline: test-pipeline
  annotations: {}
spec:
  pipelineRef:
    name: test-pipeline
status:
  runs: {}
  pipelineSpec:
    results:
    - description: pipeline result
      name: result
      value: $(finally.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(finally.b-task.results.b-Result)
    tasks:
    - name: c-task
      taskRef:
        name: c-task
        kind: Task
    finally:
    - name: a-task
      taskRef:
        name: a-task
        kind: Task
    - name: b-task
      taskRef:
        name: b-task
        apiVersion: example.dev/v0
        kind: Example
  conditions:
  - status: "True"
    type: Succeeded
    reason: Succeeded
    message: "Tasks Completed: 3 (Failed: 0, Cancelled 0), Skipped: 0"
  pipelineResults:
  - name: result
    value: aResultValue
  - name: custom-result
    value: bResultValue
  taskRuns: {}
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: test-pipeline-run-finally-results-task-run-c
    pipelineTaskName: c-task
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: test-pipeline-run-finally-results-task-run-a
    pipelineTaskName: a-task
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: test-pipeline-run-finally-results-task-run-b
    pipelineTaskName: b-task
`)
	var expectedPr *v1beta1.PipelineRun
	switch {
	case embeddedStatus == config.BothEmbeddedStatus:
		expectedPr = expectedPrBothStatus
	case embeddedStatus == config.DefaultEmbeddedStatus:
		expectedPr = expectedPrFullStatus
	case embeddedStatus == config.FullEmbeddedStatus:
		expectedPr = expectedPrFullStatus
	case embeddedStatus == config.MinimalEmbeddedStatus:
		expectedPr = expectedPrMinimalStatus
	}

	if d := cmp.Diff(expectedPr, reconciledRun, ignoreResourceVersion, ignoreLastTransitionTime, ignoreCompletionTime, ignoreStartTime); d != "" {
		t.Errorf("expected to see pipeline run results created. Diff %s", diff.PrintWantGot(d))
	}

}

func runTestReconcileWithPipelineResults(t *testing.T, embeddedStatus string) {
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  results:
    - description: pipeline result
      name: result
      value: $(tasks.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(tasks.b-task.results.b-Result)
  tasks:
  - name: b-task
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
      name: b-task
  - name: a-task
    taskRef:
      name: a-task
`)}
	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-results-task-run-a", "foo",
			"test-pipeline-run-results", "test-pipeline", "a-task", true),
		`
spec:
  taskRef:
    name: hello-world
status:
  conditions:
  - status: "True"
    type: Succeeded
  taskResults:
  - name: a-Result
    value: aResultValue
`)}

	rs := []*v1alpha1.Run{mustParseRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-results-task-run-b", "foo",
			"test-pipeline-run-results", "test-pipeline", "b-task", true),
		`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
status:
  conditions:
  - status: "True"
    type: Succeeded
  results:
  - name: b-Result
    value: bResultValue
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-results
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
status:
  conditions:
  - status: "Unknown"
    type: Succeeded
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: a-task
  namespace: foo
spec:
  results:
  - name: a-Result
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
		Runs:         rs,
		ConfigMaps:   []*corev1.ConfigMap{withCustomTasks(withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus))},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()
	reconciledRun, _ := prt.reconcileRun("foo", "test-pipeline-run-results", []string{}, false)

	expectedPrFullStatus := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-results
  namespace: foo
  labels:
    tekton.dev/pipeline: test-pipeline
  annotations: {}
spec:
  pipelineRef:
    name: test-pipeline
status:
  pipelineSpec:
    results:
    - description: pipeline result
      name: result
      value: $(tasks.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(tasks.b-task.results.b-Result)
    tasks:
    - name: b-task
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
        name: b-task
    - name: a-task
      taskRef:
        name: a-task
        kind: Task
  conditions:
  - status: "True"
    type: Succeeded
    reason: Succeeded
    message: "Tasks Completed: 2 (Failed: 0, Cancelled 0), Skipped: 0"
  pipelineResults:
  - name: result
    value: aResultValue
  - name: custom-result
    value: bResultValue
  taskRuns:
    test-pipeline-run-results-task-run-a:
      pipelineTaskName: a-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        taskResults:
        - name: a-Result
          value: aResultValue
  runs:
    test-pipeline-run-results-task-run-b:
      pipelineTaskName: b-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        results:
        - name: b-Result
          value: bResultValue
`)

	expectedPrBothStatus := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-results
  namespace: foo
  labels:
    tekton.dev/pipeline: test-pipeline
  annotations: {}
spec:
  pipelineRef:
    name: test-pipeline
status:
  pipelineSpec:
    results:
    - description: pipeline result
      name: result
      value: $(tasks.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(tasks.b-task.results.b-Result)
    tasks:
    - name: b-task
      taskRef:
        name: b-task
        apiVersion: example.dev/v0
        kind: Example
    - name: a-task
      taskRef:
        name: a-task
        kind: Task
  conditions:
  - status: "True"
    type: Succeeded
    reason: Succeeded
    message: "Tasks Completed: 2 (Failed: 0, Cancelled 0), Skipped: 0"
  pipelineResults:
  - name: result
    value: aResultValue
  - name: custom-result
    value: bResultValue
  childReferences:
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: test-pipeline-run-results-task-run-b
    pipelineTaskName: b-task
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: test-pipeline-run-results-task-run-a
    pipelineTaskName: a-task
  taskRuns:
    test-pipeline-run-results-task-run-a:
      pipelineTaskName: a-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        taskResults:
        - name: a-Result
          value: aResultValue
  runs:
    test-pipeline-run-results-task-run-b:
      pipelineTaskName: b-task
      status:
        conditions:
        - status: "True"
          type: Succeeded
        results:
        - name: b-Result
          value: bResultValue
`)

	expectedPrMinimalStatus := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-results
  namespace: foo
  labels:
    tekton.dev/pipeline: test-pipeline
  annotations: {}
spec:
  pipelineRef:
    name: test-pipeline
status:
  runs: {}
  pipelineSpec:
    results:
    - description: pipeline result
      name: result
      value: $(tasks.a-task.results.a-Result)
    - description: custom task pipeline result
      name: custom-result
      value: $(tasks.b-task.results.b-Result)
    tasks:
    - name: b-task
      taskRef:
        name: b-task
        apiVersion: example.dev/v0
        kind: Example
    - name: a-task
      taskRef:
        name: a-task
        kind: Task
  conditions:
  - status: "True"
    type: Succeeded
    reason: Succeeded
    message: "Tasks Completed: 2 (Failed: 0, Cancelled 0), Skipped: 0"
  pipelineResults:
  - name: result
    value: aResultValue
  - name: custom-result
    value: bResultValue
  taskRuns: {}
  childReferences:
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: test-pipeline-run-results-task-run-b
    pipelineTaskName: b-task
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: test-pipeline-run-results-task-run-a
    pipelineTaskName: a-task
`)
	var expectedPr *v1beta1.PipelineRun
	switch {
	case embeddedStatus == config.BothEmbeddedStatus:
		expectedPr = expectedPrBothStatus
	case embeddedStatus == config.DefaultEmbeddedStatus:
		expectedPr = expectedPrFullStatus
	case embeddedStatus == config.FullEmbeddedStatus:
		expectedPr = expectedPrFullStatus
	case embeddedStatus == config.MinimalEmbeddedStatus:
		expectedPr = expectedPrMinimalStatus
	}

	if d := cmp.Diff(expectedPr, reconciledRun, ignoreResourceVersion, ignoreLastTransitionTime, ignoreCompletionTime, ignoreStartTime); d != "" {
		t.Errorf("expected to see pipeline run results created. Diff %s", diff.PrintWantGot(d))
	}

}

func TestReconcileWithPipelineResults_OnFailedPipelineRun(t *testing.T) {
	testCases := []struct {
		name        string
		embeddedVal string
	}{
		{
			name:        "default embedded status",
			embeddedVal: config.DefaultEmbeddedStatus,
		},
		{
			name:        "full embedded status",
			embeddedVal: config.FullEmbeddedStatus,
		},
		{
			name:        "both embedded status",
			embeddedVal: config.BothEmbeddedStatus,
		},
		{
			name:        "minimal embedded status",
			embeddedVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileWithPipelineResultsOnFailedPipelineRun(t, tc.embeddedVal)
		})
	}
}

func runTestReconcileWithPipelineResultsOnFailedPipelineRun(t *testing.T, embeddedStatus string) {
	names.TestingSeed()
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  results:
  - description: pipeline result
    name: result
    value: $(tasks.a-task.results.aResult)
  tasks:
  - name: a-task
    taskRef:
      name: a-task
  - name: b-task
    params:
    - name: bParam
      value: $(tasks.a-task.results.aResult)
    taskRef:
      name: b-task
`)}
	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-failed-pr-with-task-results-a-task", "foo",
			"test-failed-pr-with-task-results", "test-pipeline", "a-task", true),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
  timeout: 1h0m0s
status:
  conditions:
  - status: "True"
    type: Succeeded
  taskResults:
  - name: aResult
    value: aResultValue
`), mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-failed-pr-with-task-results-b-task", "foo",
			"test-failed-pr-with-task-results", "test-pipeline", "b-task", true),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
     name: hello-world
  timeout: 1h0m0s
status:
  conditions:
  - status: "False"
    type: Succeeded
  taskResults:
  - name: bResult
    value: bResultValue
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-failed-pr-with-task-results
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
  timeouts:
    pipeline: "0"
status:
  conditions:
  - message: Message
    reason: Running
    status: "Unknown"
    type: Succeeded
  startTime: "2021-12-31T00:00:00Z"
`)}
	ts := []*v1beta1.Task{
		{ObjectMeta: baseObjectMeta("a-task", "foo")},
		parse.MustParseTask(t, `
metadata:
  name: b-task
  namespace: foo
spec:
  params:
  - name: bParam
    type: string
`),
	}
	wantPrs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-failed-pr-with-task-results
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
  timeouts:
    pipeline: "0"
status:
  conditions:
  - message: "Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0"
    reason: Failed
    status: "False"
    type: Succeeded
  pipelineResults:
    - name: result
      value: aResultValue
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
		ConfigMaps:   []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, _ := prt.reconcileRun("foo", "test-failed-pr-with-task-results", []string{}, false)

	if d := cmp.Diff(reconciledRun.Status.Conditions, wantPrs[0].Status.Conditions, ignoreResourceVersion, ignoreLastTransitionTime); d != "" {
		t.Errorf("expected to see pipeline run marked as failed. Diff %s", diff.PrintWantGot(d))
	}
	if d := cmp.Diff(reconciledRun.Status.PipelineResults, wantPrs[0].Status.PipelineResults, ignoreResourceVersion, ignoreLastTransitionTime); d != "" {
		t.Errorf("expected to see pipeline run results created. Diff %s", diff.PrintWantGot(d))
	}
}

func Test_storePipelineSpec(t *testing.T) {
	pr := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-success
  labels:
    lbl: value
  annotations:
    io.annotation: value
`)

	ps := v1beta1.PipelineSpec{Description: "foo-pipeline"}
	ps1 := v1beta1.PipelineSpec{Description: "bar-pipeline"}

	want := pr.DeepCopy()
	want.Status = v1beta1.PipelineRunStatus{
		PipelineRunStatusFields: v1beta1.PipelineRunStatusFields{
			PipelineSpec: ps.DeepCopy(),
		},
	}
	want.ObjectMeta.Labels["tekton.dev/pipeline"] = pr.ObjectMeta.Name

	// The first time we set it, it should get copied.
	if err := storePipelineSpecAndMergeMeta(pr, &ps, &pr.ObjectMeta); err != nil {
		t.Errorf("storePipelineSpec() error = %v", err)
	}
	if d := cmp.Diff(pr, want); d != "" {
		t.Fatalf(diff.PrintWantGot(d))
	}

	// The next time, it should not get overwritten
	if err := storePipelineSpecAndMergeMeta(pr, &ps1, &metav1.ObjectMeta{}); err != nil {
		t.Errorf("storePipelineSpec() error = %v", err)
	}
	if d := cmp.Diff(pr, want); d != "" {
		t.Fatalf(diff.PrintWantGot(d))
	}
}

func Test_storePipelineSpec_metadata(t *testing.T) {
	pipelinerunlabels := map[string]string{"lbl1": "value1", "lbl2": "value2"}
	pipelinerunannotations := map[string]string{"io.annotation.1": "value1", "io.annotation.2": "value2"}
	pipelinelabels := map[string]string{"lbl1": "another value", "lbl2": "another value", "lbl3": "value3"}
	pipelineannotations := map[string]string{"io.annotation.1": "another value", "io.annotation.2": "another value", "io.annotation.3": "value3"}
	wantedlabels := map[string]string{"lbl1": "value1", "lbl2": "value2", "lbl3": "value3", pipeline.PipelineLabelKey: "bar"}
	wantedannotations := map[string]string{"io.annotation.1": "value1", "io.annotation.2": "value2", "io.annotation.3": "value3"}

	pr := &v1beta1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Labels: pipelinerunlabels, Annotations: pipelinerunannotations},
	}
	meta := metav1.ObjectMeta{Name: "bar", Labels: pipelinelabels, Annotations: pipelineannotations}
	if err := storePipelineSpecAndMergeMeta(pr, &v1beta1.PipelineSpec{}, &meta); err != nil {
		t.Errorf("storePipelineSpecAndMergeMeta error = %v", err)
	}
	if d := cmp.Diff(pr.ObjectMeta.Labels, wantedlabels); d != "" {
		t.Fatalf(diff.PrintWantGot(d))
	}
	if d := cmp.Diff(pr.ObjectMeta.Annotations, wantedannotations); d != "" {
		t.Fatalf(diff.PrintWantGot(d))
	}
}

func TestReconcileOutOfSyncPipelineRun(t *testing.T) {
	// It may happen that a PipelineRun creates one or more TaskRuns during reconcile
	// but it fails to sync the update on the status back. This test verifies that
	// the reconciler is able to coverge back to a consistent state with the orphaned
	// TaskRuns back in the PipelineRun status.
	// For more details, see https://github.com/tektoncd/pipeline/issues/2558

	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileOutOfSyncPipelineRun(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileOutOfSyncPipelineRun(t *testing.T, embeddedStatus string) {
	ctx := context.Background()

	prOutOfSyncName := "test-pipeline-run-out-of-sync"
	helloWorldTask := simpleHelloWorldTask

	testPipeline := parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: hello-world-1
    taskRef:
      name: hello-world
  - name: hello-world-2
    taskRef:
      name: hello-world
  - name: hello-world-5
    taskRef:
      apiVersion: example.dev/v0
      kind: Example
`)

	// This taskrun is in the pipelinerun status. It completed successfully.
	taskRunDone := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-out-of-sync-hello-world-1", "foo", prOutOfSyncName, testPipeline.Name, "hello-world-1", false),
		`
spec:
  taskRef:
    name: hello-world
status:
  conditions:
  - status: "True"
    type: Succeeded
`)

	// This taskrun is *not* in the pipelinerun status. It's still running.
	taskRunOrphaned := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-out-of-sync-hello-world-2", "foo", prOutOfSyncName, testPipeline.Name, "hello-world-2", false),
		`
spec:
  taskRef:
    name: hello-world
status:
  conditions:
  - status: Unknown
    type: Succeeded
`)

	orphanedRun := mustParseRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-out-of-sync-hello-world-5", "foo", prOutOfSyncName, testPipeline.Name,
			"hello-world-5", true),
		`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
status:
  conditions:
  - status: Unknown
    type: Succeeded
`)

	prOutOfSync := parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-out-of-sync
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
status:
  conditions:
  - status: Unknown
    type: Succeeded
`)

	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		prOutOfSync.Status.TaskRuns = map[string]*v1beta1.PipelineRunTaskRunStatus{
			taskRunDone.Name: {
				PipelineTaskName: "hello-world-1",
				Status:           &v1beta1.TaskRunStatus{},
			},
		}
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		prOutOfSync.Status.ChildReferences = []v1beta1.ChildStatusReference{
			{
				TypeMeta: runtime.TypeMeta{
					APIVersion: v1beta1.SchemeGroupVersion.String(),
					Kind:       "TaskRun",
				},
				Name:             taskRunDone.Name,
				PipelineTaskName: "hello-world-1",
			},
		}
	}

	prs := []*v1beta1.PipelineRun{prOutOfSync}
	ps := []*v1beta1.Pipeline{testPipeline}
	ts := []*v1beta1.Task{helloWorldTask}
	trs := []*v1beta1.TaskRun{taskRunDone, taskRunOrphaned}
	runs := []*v1alpha1.Run{orphanedRun}

	cms := []*corev1.ConfigMap{withCustomTasks(withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus))}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
		Runs:         runs,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", prOutOfSync.Name, []string{}, false)

	actions := clients.Pipeline.Actions()
	if len(actions) < 3 {
		t.Fatalf("Expected client to have at least three action implementation but it has %d", len(actions))
	}

	_ = getPipelineRunUpdates(t, actions)
	pipelineUpdates := 0
	for _, action := range actions {
		if action != nil {
			switch {
			case action.Matches("create", "taskruns"):
				t.Errorf("Expected client to not have created a TaskRun, but it did")
			case action.Matches("create", "runs"):
				t.Errorf("Expected client to not have created a Run, but it did")
			case action.Matches("update", "pipelineruns"):
				pipelineUpdates++
			case action.Matches("patch", "pipelineruns"):
				pipelineUpdates++
			default:
				continue
			}
		}
	}

	// We actually expect three update calls because the first status update fails due to
	// optimistic concurrency (due to the label update) and is retried after reloading via
	// the client.
	if got, want := pipelineUpdates, 3; got != want {
		// If only the pipelinerun status changed, we expect one update
		t.Fatalf("Expected client to have updated the pipelinerun %d times, but it did %d times", want, got)
	}

	// This PipelineRun should still be running and the status should reflect that
	if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
		t.Errorf("Expected PipelineRun status to be running, but was %v", reconciledRun.Status.GetCondition(apis.ConditionSucceeded))
	}

	expectedTaskRunsStatus := make(map[string]*v1beta1.PipelineRunTaskRunStatus)
	expectedRunsStatus := make(map[string]*v1beta1.PipelineRunRunStatus)
	// taskRunDone did not change
	expectedTaskRunsStatus[taskRunDone.Name] = &v1beta1.PipelineRunTaskRunStatus{
		PipelineTaskName: "hello-world-1",
		Status: &v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{
				Conditions: []apis.Condition{
					{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					},
				},
			},
		},
	}
	// taskRunOrphaned was recovered into the status
	expectedTaskRunsStatus[taskRunOrphaned.Name] = &v1beta1.PipelineRunTaskRunStatus{
		PipelineTaskName: "hello-world-2",
		Status: &v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{
				Conditions: []apis.Condition{
					{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionUnknown,
					},
				},
			},
		},
	}
	// orphanedRun was recovered into the status
	expectedRunsStatus[orphanedRun.Name] = &v1beta1.PipelineRunRunStatus{
		PipelineTaskName: "hello-world-5",
		Status: &v1alpha1.RunStatus{
			Status: duckv1.Status{
				Conditions: []apis.Condition{
					{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionUnknown,
					},
				},
			},
		},
	}

	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		if d := cmp.Diff(expectedTaskRunsStatus, reconciledRun.Status.TaskRuns); d != "" {
			t.Fatalf("Expected PipelineRun status to match TaskRun(s) status, but got a mismatch: %s", d)
		}
		if d := cmp.Diff(expectedRunsStatus, reconciledRun.Status.Runs); d != "" {
			t.Fatalf("Expected PipelineRun status to match Run(s) status, but got a mismatch: %s", d)
		}
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		taskRunsStatus := make(map[string]*v1beta1.PipelineRunTaskRunStatus)
		runsStatus := make(map[string]*v1beta1.PipelineRunRunStatus)

		for _, cr := range reconciledRun.Status.ChildReferences {
			if cr.Kind == "TaskRun" {
				trStatusForPipelineRun := &v1beta1.PipelineRunTaskRunStatus{
					PipelineTaskName: cr.PipelineTaskName,
					WhenExpressions:  cr.WhenExpressions,
				}

				tr, _ := clients.Pipeline.TektonV1beta1().TaskRuns("foo").Get(ctx, cr.Name, metav1.GetOptions{})
				if tr != nil {
					trStatusForPipelineRun.Status = &tr.Status
				}

				taskRunsStatus[cr.Name] = trStatusForPipelineRun
			} else if cr.Kind == "Run" {
				rStatusForPipelineRun := &v1beta1.PipelineRunRunStatus{
					PipelineTaskName: cr.PipelineTaskName,
					WhenExpressions:  cr.WhenExpressions,
				}

				r, _ := clients.Pipeline.TektonV1alpha1().Runs("foo").Get(ctx, cr.Name, metav1.GetOptions{})
				if r != nil {
					rStatusForPipelineRun.Status = &r.Status
				}

				runsStatus[cr.Name] = rStatusForPipelineRun
			}
		}
		if d := cmp.Diff(expectedTaskRunsStatus, taskRunsStatus); d != "" {
			t.Fatalf("Expected PipelineRun status to match TaskRun(s) status, but got a mismatch: %s", d)
		}
		if d := cmp.Diff(expectedRunsStatus, runsStatus); d != "" {
			t.Fatalf("Expected PipelineRun status to match Run(s) status, but got a mismatch: %s", d)
		}
	}
}

func TestUpdatePipelineRunStatusFromInformer(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestUpdatePipelineRunStatusFromInformer(t, tc.embeddedStatusVal)
		})
	}
}

func runTestUpdatePipelineRunStatusFromInformer(t *testing.T, embeddedStatus string) {
	names.TestingSeed()

	pr := parse.MustParsePipelineRun(t, `
metadata:
  labels:
    mylabel: myvale
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineSpec:
    tasks:
    - name: unit-test-task-spec
      taskSpec:
        steps:
        - image: myimage
          name: mystep
    - name: custom-task-ref
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
        name: some-custom-task
`)

	cms := []*corev1.ConfigMap{withCustomTasks(withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus))}

	d := test.Data{
		PipelineRuns: []*v1beta1.PipelineRun{pr},
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0",
	}

	// Reconcile the PipelineRun.  This creates a Taskrun.
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run", wantEvents, false)

	// Save the name of the TaskRun and Run that were created.
	taskRunName := ""
	runName := ""
	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		if len(reconciledRun.Status.TaskRuns) != 1 {
			t.Fatalf("Expected 1 TaskRun but got %d", len(reconciledRun.Status.TaskRuns))
		}
		for k := range reconciledRun.Status.TaskRuns {
			taskRunName = k
			break
		}
		if len(reconciledRun.Status.Runs) != 1 {
			t.Fatalf("Expected 1 Run but got %d", len(reconciledRun.Status.Runs))
		}
		for k := range reconciledRun.Status.Runs {
			runName = k
			break
		}
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		if len(reconciledRun.Status.ChildReferences) != 2 {
			t.Fatalf("Expected 2 ChildReferences but got %d", len(reconciledRun.Status.ChildReferences))
		}
		for _, cr := range reconciledRun.Status.ChildReferences {
			if cr.Kind == "TaskRun" {
				taskRunName = cr.Name
			}
			if cr.Kind == "Run" {
				runName = cr.Name
			}
		}
	}

	if taskRunName == "" {
		t.Fatal("expected to find a TaskRun name, but didn't")
	}
	if runName == "" {
		t.Fatal("expected to find a Run name, but didn't")
	}

	// Add a label to the PipelineRun.  This tests a scenario in issue 3126 which could prevent the reconciler
	// from finding TaskRuns that are missing from the status.
	reconciledRun.ObjectMeta.Labels["bah"] = "humbug"
	reconciledRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Update(prt.TestAssets.Ctx, reconciledRun, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("unexpected error when updating status: %v", err)
	}

	// The label update triggers another reconcile.  Depending on timing, the PipelineRun passed to the reconcile may or may not
	// have the updated status with the name of the created TaskRun.  Clear the status because we want to test the case where the
	// status does not have the TaskRun.
	reconciledRun.Status = v1beta1.PipelineRunStatus{}
	if _, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").UpdateStatus(prt.TestAssets.Ctx, reconciledRun, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("unexpected error when updating status: %v", err)
	}

	reconciledRun, _ = prt.reconcileRun("foo", "test-pipeline-run", wantEvents, false)

	// Verify that the reconciler found the existing TaskRun and Run instead of creating new ones.
	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, taskRunName)
	verifyRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
	verifyRunStatusesNames(t, embeddedStatus, reconciledRun.Status, runName)
}

func TestReconcilePipeline_FinalTasks(t *testing.T) {
	tests := []struct {
		name                     string
		pipelineRunName          string
		prs                      []*v1beta1.PipelineRun
		ps                       []*v1beta1.Pipeline
		ts                       []*v1beta1.Task
		trs                      []*v1beta1.TaskRun
		expectedTaskRuns         map[string]*v1beta1.PipelineRunTaskRunStatus
		pipelineRunStatusUnknown bool
		pipelineRunStatusFalse   bool
	}{{
		// pipeline run should result in error when a dag task is executed and resulted in failure but final task is executed successfully

		// pipelineRunName - "pipeline-run-dag-task-failing"
		// pipelineName - "pipeline-dag-task-failing"
		// pipelineTasks - "dag-task-1" and "final-task-1"
		// taskRunNames - "task-run-dag-task" and "task-run-final-task"
		// taskName - "hello-world"

		name: "Test 01 - Pipeline run should result in error when a dag task fails but final task is executed successfully.",

		pipelineRunName: "pipeline-run-dag-task-failing",

		prs: getPipelineRun(
			"pipeline-run-dag-task-failing",
			"pipeline-dag-task-failing",
			corev1.ConditionFalse,
			v1beta1.PipelineRunReasonFailed.String(),
			"Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0",
			map[string]string{
				"dag-task-1":   "task-run-dag-task",
				"final-task-1": "task-run-final-task",
			},
		),

		ps: getPipeline(
			"pipeline-dag-task-failing",
			v1beta1.PipelineSpec{
				Tasks: []v1beta1.PipelineTask{{
					Name:    "dag-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
				Finally: []v1beta1.PipelineTask{{
					Name:    "final-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
			}),

		ts: []*v1beta1.Task{simpleHelloWorldTask},

		trs: []*v1beta1.TaskRun{
			getTaskRun(
				t,
				"task-run-dag-task",
				"pipeline-run-dag-task-failing",
				"pipeline-dag-task-failing",
				"dag-task-1",
				corev1.ConditionFalse,
			),
			getTaskRun(
				t,
				"task-run-final-task",
				"pipeline-run-dag-task-failing",
				"pipeline-dag-task-failing",
				"final-task-1",
				"",
			),
		},

		expectedTaskRuns: map[string]*v1beta1.PipelineRunTaskRunStatus{
			"task-run-dag-task":   getTaskRunStatus("dag-task-1", corev1.ConditionFalse),
			"task-run-final-task": getTaskRunStatus("final-task-1", ""),
		},

		pipelineRunStatusFalse: true,
	}, {

		// pipeline run should result in error when a dag task is successful but the final task fails

		// pipelineRunName - "pipeline-run-with-dag-successful-but-final-failing"
		// pipelineName - "pipeline-with-dag-successful-but-final-failing"
		// pipelineTasks - "dag-task-1" and "final-task-1"
		// taskRunNames - "task-run-dag-task" and "task-run-final-task"
		// taskName - "hello-world"

		name: "Test 02 - Pipeline run should result in error when a dag task is successful but final task fails.",

		pipelineRunName: "pipeline-run-with-dag-successful-but-final-failing",

		prs: getPipelineRun(
			"pipeline-run-with-dag-successful-but-final-failing",
			"pipeline-with-dag-successful-but-final-failing",
			corev1.ConditionFalse,
			v1beta1.PipelineRunReasonFailed.String(),
			"Tasks Completed: 2 (Failed: 1, Cancelled 0), Skipped: 0",
			map[string]string{
				"dag-task-1":   "task-run-dag-task",
				"final-task-1": "task-run-final-task",
			},
		),

		ps: getPipeline(
			"pipeline-with-dag-successful-but-final-failing",
			v1beta1.PipelineSpec{
				Tasks: []v1beta1.PipelineTask{{
					Name:    "dag-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
				Finally: []v1beta1.PipelineTask{{
					Name:    "final-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
			}),

		ts: []*v1beta1.Task{simpleHelloWorldTask},

		trs: []*v1beta1.TaskRun{
			getTaskRun(
				t,
				"task-run-dag-task",
				"pipeline-run-with-dag-successful-but-final-failing",
				"pipeline-with-dag-successful-but-final-failing",
				"dag-task-1",
				"",
			),
			getTaskRun(
				t,
				"task-run-final-task",
				"pipeline-run-with-dag-successful-but-final-failing",
				"pipeline-with-dag-successful-but-final-failing",
				"final-task-1",
				corev1.ConditionFalse,
			),
		},

		expectedTaskRuns: map[string]*v1beta1.PipelineRunTaskRunStatus{
			"task-run-dag-task":   getTaskRunStatus("dag-task-1", ""),
			"task-run-final-task": getTaskRunStatus("final-task-1", corev1.ConditionFalse),
		},

		pipelineRunStatusFalse: true,
	}, {

		// pipeline run should result in error when a dag task and final task both are executed and resulted in failure

		// pipelineRunName - "pipeline-run-with-dag-and-final-failing"
		// pipelineName - "pipeline-with-dag-and-final-failing"
		// pipelineTasks - "dag-task-1" and "final-task-1"
		// taskRunNames - "task-run-dag-task" and "task-run-final-task"
		// taskName - "hello-world"

		name: "Test 03 - Pipeline run should result in error when both dag task and final task fail.",

		pipelineRunName: "pipeline-run-with-dag-and-final-failing",

		prs: getPipelineRun(
			"pipeline-run-with-dag-and-final-failing",
			"pipeline-with-dag-and-final-failing",
			corev1.ConditionFalse,
			v1beta1.PipelineRunReasonFailed.String(),
			"Tasks Completed: 2 (Failed: 2, Cancelled 0), Skipped: 0",
			map[string]string{
				"dag-task-1":   "task-run-dag-task",
				"final-task-1": "task-run-final-task",
			},
		),

		ps: getPipeline(
			"pipeline-with-dag-and-final-failing",
			v1beta1.PipelineSpec{
				Tasks: []v1beta1.PipelineTask{{
					Name:    "dag-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
				Finally: []v1beta1.PipelineTask{{
					Name:    "final-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
			}),

		ts: []*v1beta1.Task{simpleHelloWorldTask},

		trs: []*v1beta1.TaskRun{
			getTaskRun(
				t,
				"task-run-dag-task",
				"pipeline-run-with-dag-and-final-failing",
				"pipeline-with-dag-and-final-failing",
				"dag-task-1",
				corev1.ConditionFalse,
			),
			getTaskRun(
				t,
				"task-run-final-task",
				"pipeline-run-with-dag-and-final-failing",
				"pipeline-with-dag-and-final-failing",
				"final-task-1",
				corev1.ConditionFalse,
			),
		},

		expectedTaskRuns: map[string]*v1beta1.PipelineRunTaskRunStatus{
			"task-run-dag-task":   getTaskRunStatus("dag-task-1", corev1.ConditionFalse),
			"task-run-final-task": getTaskRunStatus("final-task-1", corev1.ConditionFalse),
		},

		pipelineRunStatusFalse: true,
	}, {

		// pipeline run should not schedule final tasks until dag tasks are done i.e.
		// dag task 1 fails but dag task 2 is still running, pipeline run should not schedule and create task run for final task

		// pipelineRunName - "pipeline-run-with-dag-running"
		// pipelineName - "pipeline-with-dag-running"
		// pipelineTasks - "dag-task-1", "dag-task-2" and "final-task-1"
		// taskRunNames - "task-run-dag-task-1" and "task-run-dag-task-2" - no task run for final task
		// taskName - "hello-world"

		name: "Test 04 - Pipeline run should not schedule final tasks while dag tasks are still running.",

		pipelineRunName: "pipeline-run-with-dag-running",

		prs: getPipelineRun(
			"pipeline-run-with-dag-running",
			"pipeline-with-dag-running",
			corev1.ConditionUnknown,
			v1beta1.PipelineRunReasonRunning.String(),
			"Tasks Completed: 1 (Failed: 1, Cancelled 0), Incomplete: 2, Skipped: 0",
			map[string]string{
				"dag-task-1": "task-run-dag-task-1",
				"dag-task-2": "task-run-dag-task-2",
			},
		),

		ps: getPipeline(
			"pipeline-with-dag-running",
			v1beta1.PipelineSpec{
				Tasks: []v1beta1.PipelineTask{
					{
						Name:    "dag-task-1",
						TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
					},
					{
						Name:    "dag-task-2",
						TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
					},
				},
				Finally: []v1beta1.PipelineTask{{
					Name:    "final-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
			}),

		ts: []*v1beta1.Task{simpleHelloWorldTask},

		trs: []*v1beta1.TaskRun{
			getTaskRun(
				t,
				"task-run-dag-task-1",
				"pipeline-run-with-dag-running",
				"pipeline-with-dag-running",
				"dag-task-1",
				corev1.ConditionFalse,
			),
			getTaskRun(
				t,
				"task-run-dag-task-2",
				"pipeline-run-with-dag-running",
				"pipeline-with-dag-running",
				"dag-task-2",
				corev1.ConditionUnknown,
			),
		},

		expectedTaskRuns: map[string]*v1beta1.PipelineRunTaskRunStatus{
			"task-run-dag-task-1": getTaskRunStatus("dag-task-1", corev1.ConditionFalse),
			"task-run-dag-task-2": getTaskRunStatus("dag-task-2", corev1.ConditionUnknown),
		},

		pipelineRunStatusUnknown: true,
	}, {

		// pipeline run should not schedule final tasks until dag tasks are done i.e.
		// dag task is still running and no other dag task available to schedule,
		// pipeline run should not schedule and create task run for final task

		// pipelineRunName - "pipeline-run-dag-task-running"
		// pipelineName - "pipeline-dag-task-running"
		// pipelineTasks - "dag-task-1" and "final-task-1"
		// taskRunNames - "task-run-dag-task-1" - no task run for final task
		// taskName - "hello-world"

		name: "Test 05 - Pipeline run should not schedule final tasks while dag tasks are still running and no other dag task available to schedule.",

		pipelineRunName: "pipeline-run-dag-task-running",

		prs: getPipelineRun(
			"pipeline-run-dag-task-running",
			"pipeline-dag-task-running",
			corev1.ConditionUnknown,
			v1beta1.PipelineRunReasonRunning.String(),
			"Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0",
			map[string]string{
				"dag-task-1": "task-run-dag-task-1",
			},
		),

		ps: getPipeline(
			"pipeline-dag-task-running",
			v1beta1.PipelineSpec{
				Tasks: []v1beta1.PipelineTask{{
					Name:    "dag-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
				Finally: []v1beta1.PipelineTask{{
					Name:    "final-task-1",
					TaskRef: &v1beta1.TaskRef{Name: "hello-world"},
				}},
			}),

		ts: []*v1beta1.Task{simpleHelloWorldTask},

		trs: []*v1beta1.TaskRun{
			getTaskRun(
				t,
				"task-run-dag-task-1",
				"pipeline-run-dag-task-running",
				"pipeline-dag-task-running",
				"dag-task-1",
				corev1.ConditionUnknown,
			),
		},

		expectedTaskRuns: map[string]*v1beta1.PipelineRunTaskRunStatus{
			"task-run-dag-task-1": getTaskRunStatus("dag-task-1", corev1.ConditionUnknown),
		},

		pipelineRunStatusUnknown: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := test.Data{
				PipelineRuns: tt.prs,
				Pipelines:    tt.ps,
				Tasks:        tt.ts,
				TaskRuns:     tt.trs,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			reconciledRun, clients := prt.reconcileRun("foo", tt.pipelineRunName, []string{}, false)

			actions := clients.Pipeline.Actions()
			if len(actions) < 2 {
				t.Fatalf("Expected client to have at least two action implementation but it has %d", len(actions))
			}

			// The first update action should be updating the PipelineRun.
			var actual *v1beta1.PipelineRun
			for _, action := range actions {
				if actualPrime, ok := action.(ktesting.UpdateAction); ok {
					actual = actualPrime.GetObject().(*v1beta1.PipelineRun)
					break
				}
			}

			if actual == nil {
				t.Errorf("Expected a PipelineRun to be updated, but it wasn't for %s", tt.name)
			}

			for _, action := range actions {
				if action != nil {
					resource := action.GetResource().Resource
					if resource == "taskruns" {
						t.Fatalf("Expected client to not have created a TaskRun for the PipelineRun, but it did for %s", tt.name)
					}
				}
			}

			if tt.pipelineRunStatusFalse {
				// This PipelineRun should still be failed and the status should reflect that
				if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsFalse() {
					t.Errorf("Expected PipelineRun status to be failed, but was %v for %s",
						reconciledRun.Status.GetCondition(apis.ConditionSucceeded), tt.name)
				}
			} else if tt.pipelineRunStatusUnknown {
				// This PipelineRun should still be running and the status should reflect that
				if !reconciledRun.Status.GetCondition(apis.ConditionSucceeded).IsUnknown() {
					t.Errorf("Expected PipelineRun status to be unknown (running), but was %v for %s",
						reconciledRun.Status.GetCondition(apis.ConditionSucceeded), tt.name)
				}
			}

			if d := cmp.Diff(reconciledRun.Status.TaskRuns, tt.expectedTaskRuns); d != "" {
				t.Fatalf("Expected PipelineRunTaskRun status to match TaskRun(s) status, but got a mismatch for %s: %s", tt.name, d)
			}

		})
	}
}

func getPipelineRun(pr, p string, status corev1.ConditionStatus, reason string, m string, tr map[string]string) []*v1beta1.PipelineRun {
	pRun := &v1beta1.PipelineRun{
		ObjectMeta: baseObjectMeta(pr, "foo"),
		Spec: v1beta1.PipelineRunSpec{
			PipelineRef:        &v1beta1.PipelineRef{Name: p},
			ServiceAccountName: "test-sa",
		},
		Status: v1beta1.PipelineRunStatus{
			Status: duckv1beta1.Status{
				Conditions: duckv1beta1.Conditions{
					apis.Condition{
						Type:    apis.ConditionSucceeded,
						Status:  status,
						Reason:  reason,
						Message: m,
					},
				},
			},
			PipelineRunStatusFields: v1beta1.PipelineRunStatusFields{TaskRuns: map[string]*v1beta1.PipelineRunTaskRunStatus{}},
		},
	}
	for k, v := range tr {
		pRun.Status.TaskRuns[v] = &v1beta1.PipelineRunTaskRunStatus{PipelineTaskName: k, Status: &v1beta1.TaskRunStatus{}}
	}
	return []*v1beta1.PipelineRun{pRun}
}

func getPipeline(p string, spec v1beta1.PipelineSpec) []*v1beta1.Pipeline {
	ps := []*v1beta1.Pipeline{{
		ObjectMeta: baseObjectMeta(p, "foo"),
		Spec:       spec,
	}}
	return ps
}

func getTaskRun(t *testing.T, tr, pr, p, tl string, status corev1.ConditionStatus) *v1beta1.TaskRun {
	return createHelloWorldTaskRunWithStatusTaskLabel(t, tr, "foo", pr, p, "", tl,
		apis.Condition{
			Type:   apis.ConditionSucceeded,
			Status: status,
		})
}

func getTaskRunStatus(t string, status corev1.ConditionStatus) *v1beta1.PipelineRunTaskRunStatus {
	return &v1beta1.PipelineRunTaskRunStatus{
		PipelineTaskName: t,
		Status: &v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{
				Conditions: []apis.Condition{
					{Type: apis.ConditionSucceeded, Status: status},
				},
			},
		},
	}
}

// TestReconcile_CloudEvents runs reconcile with a cloud event sink configured
// to ensure that events are sent in different cases
func TestReconcile_CloudEvents(t *testing.T) {
	names.TestingSeed()

	prs := []*v1beta1.PipelineRun{
		parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipelinerun
  namespace: foo
  selfLink: /pipeline/1234
spec:
  pipelineRef:
    name: test-pipeline
`),
	}
	ps := []*v1beta1.Pipeline{
		parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
    - name: test-1
      taskRef:
        name: test-task
`),
	}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: test-task
  namespace: foo
spec:
  steps:
    - name: simple-step
      image: foo
      command: ["/mycmd"]
      env:
       - name: foo
         value: bar
`),
	}
	cms := []*corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{Name: config.GetDefaultsConfigName(), Namespace: system.Namespace()},
			Data: map[string]string{
				"default-cloud-events-sink": "http://synk:8080",
			},
		},
	}
	t.Logf("config maps: %s", cms)

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		ConfigMaps:   cms,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipelinerun", wantEvents, false)

	// This PipelineRun is in progress now and the status should reflect that
	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())

	verifyTaskRunStatusesCount(t, cms[0].Data[embeddedStatusFeatureFlag], reconciledRun.Status, 1)

	wantCloudEvents := []string{
		`(?s)dev.tekton.event.pipelinerun.started.v1.*test-pipelinerun`,
		`(?s)dev.tekton.event.pipelinerun.running.v1.*test-pipelinerun`,
	}
	ceClient := clients.CloudEvents.(cloudevent.FakeClient)
	if err := eventstest.CheckEventsUnordered(t, ceClient.Events, "reconcile-cloud-events", wantCloudEvents); err != nil {
		t.Errorf(err.Error())
	}
}

// this test validates taskSpec metadata is embedded into task run
func TestReconcilePipeline_TaskSpecMetadata(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcilePipelineTaskSpecMetadata(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcilePipelineTaskSpecMetadata(t *testing.T, embeddedStatus string) {
	names.TestingSeed()

	prs := []*v1beta1.PipelineRun{
		parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-success
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
`),
	}

	ps := []*v1beta1.Pipeline{
		parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
    - name: task-without-metadata
      taskSpec:
        steps:
          - name: mystep
            image: myimage
    - name: task-with-metadata
      taskSpec:
        steps:
          - name: mystep
            image: myimage
        metadata:
          labels:
            label1: labelvalue1
            label2: labelvalue2
          annotations:
            annotation1: value1
            annotation2: value2
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		ConfigMaps:   []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-success", []string{}, false)

	actions := clients.Pipeline.Actions()
	if len(actions) == 0 {
		t.Fatalf("Expected client to have been used to create a TaskRun but it wasn't")
	}

	actualTaskRun := make(map[string]*v1beta1.TaskRun)
	for _, a := range actions {
		if a.GetResource().Resource == "taskruns" {
			t := a.(ktesting.CreateAction).GetObject().(*v1beta1.TaskRun)
			actualTaskRun[t.Name] = t
		}
	}

	// Check that the expected TaskRun was created
	if len(actualTaskRun) != 2 {
		t.Errorf("Expected two TaskRuns to be created, but found %d TaskRuns.", len(actualTaskRun))
	}

	expectedTaskRun := make(map[string]*v1beta1.TaskRun)
	expectedTaskRun["test-pipeline-run-success-task-with-metadata"] = getTaskRunWithTaskSpec(
		"test-pipeline-run-success-task-with-metadata",
		"test-pipeline-run-success",
		"test-pipeline",
		"task-with-metadata",
		map[string]string{"label1": "labelvalue1", "label2": "labelvalue2"},
		map[string]string{"annotation1": "value1", "annotation2": "value2"},
	)

	expectedTaskRun["test-pipeline-run-success-task-without-metadata"] = getTaskRunWithTaskSpec(
		"test-pipeline-run-success-task-without-metadata",
		"test-pipeline-run-success",
		"test-pipeline",
		"task-without-metadata",
		map[string]string{},
		map[string]string{},
	)

	if d := cmp.Diff(expectedTaskRun, actualTaskRun); d != "" {
		t.Fatalf("Expected TaskRuns to match, but got a mismatch: %s", d)
	}

	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 2)
}

func TestReconciler_ReconcileKind_PipelineTaskContext(t *testing.T) {
	names.TestingSeed()

	pipelineName := "p-pipelinetask-status"
	pipelineRunName := "pr-pipelinetask-status"

	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: p-pipelinetask-status
  namespace: foo
spec:
  finally:
  - name: finaltask
    params:
    - name: pipelineRun-tasks-task1
      value: $(tasks.task1.status)
    taskRef:
      name: finaltask
  tasks:
  - name: task1
    taskRef:
      name: mytask
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: pr-pipelinetask-status
  namespace: foo
spec:
  pipelineRef:
    name: p-pipelinetask-status
  serviceAccountName: test-sa
`)}

	ts := []*v1beta1.Task{
		{ObjectMeta: baseObjectMeta("mytask", "foo")},
		parse.MustParseTask(t, `
metadata:
  name: finaltask
  namespace: foo
spec:
  params:
  - name: pipelineRun-tasks-task1
    type: string
`)}

	trs := []*v1beta1.TaskRun{mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta(pipelineRunName+"-task1-xxyy", "foo", pipelineRunName, pipelineName, "task1", false),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - reason: Failed
    status: "False"
    type: Succeeded
`)}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", pipelineRunName, []string{}, false)

	expectedTaskRunName := pipelineRunName + "-finaltask"
	expectedTaskRunObjectMeta := taskRunObjectMeta(expectedTaskRunName, "foo", pipelineRunName, pipelineName, "finaltask", false)
	expectedTaskRunObjectMeta.Labels[pipeline.MemberOfLabelKey] = v1beta1.PipelineFinallyTasks
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t, expectedTaskRunObjectMeta, `
spec:
  params:
  - name: pipelineRun-tasks-task1
    value: Failed
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: finaltask
    kind: Task
`)
	// Check that the expected TaskRun was created
	actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
		LabelSelector: "tekton.dev/pipelineTask=finaltask,tekton.dev/pipelineRun=" + pipelineRunName,
		Limit:         1,
	})

	if err != nil {
		t.Fatalf("Failure to list TaskRun's %s", err)
	}
	if len(actual.Items) != 1 {
		t.Fatalf("Expected 1 TaskRuns got %d", len(actual.Items))
	}
	actualTaskRun := actual.Items[0]
	if d := cmp.Diff(expectedTaskRun, &actualTaskRun, ignoreResourceVersion, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRunName, diff.PrintWantGot(d))
	}
}

func TestReconcileWithTaskResultsInFinalTasks(t *testing.T) {
	names.TestingSeed()

	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  finally:
  - name: final-task-1
    params:
    - name: finalParam
      value: $(tasks.dag-task-1.results.aResult)
    taskRef:
      name: final-task
  - name: final-task-2
    params:
    - name: finalParam
      value: $(tasks.dag-task-2.results.aResult)
    taskRef:
      name: final-task
  - name: final-task-3
    params:
    - name: finalParam
      value: param
    taskRef:
      name: final-task
    when:
    - input: $(tasks.dag-task-1.results.aResult)
      operator: notin
      values:
      - aResultValue
  - name: final-task-4
    params:
    - name: finalParam
      value: param
    taskRef:
      name: final-task
    when:
    - input: $(tasks.dag-task-1.results.aResult)
      operator: in
      values:
      - aResultValue
  - name: final-task-5
    params:
    - name: finalParam
      value: param
    taskRef:
      name: final-task
    when:
    - input: $(tasks.dag-task-2.results.aResult)
      operator: notin
      values:
      - aResultValue
  - name: final-task-6
    params:
    - name: finalParam
      value: $(tasks.dag-task-2.results.aResult)
    taskRef:
      name: final-task
    when:
    - input: $(tasks.dag-task-1.results.aResult)
      operator: in
      values:
      - aResultValue
  tasks:
  - name: dag-task-1
    taskRef:
      name: dag-task
  - name: dag-task-2
    taskRef:
      name: dag-task
`)}

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-final-task-results
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa-0
`)}

	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: dag-task
  namespace: foo
`),
		parse.MustParseTask(t, `
metadata:
  name: final-task
  namespace: foo
spec:
  params:
  - name: finalParam
    type: string
`),
	}

	trs := []*v1beta1.TaskRun{
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("test-pipeline-run-final-task-results-dag-task-1-xxyyy", "foo",
				"test-pipeline-run-final-task-results", "test-pipeline", "dag-task-1", false),
			`
spec:
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
status:
  conditions:
  - reason: Succeeded
    status: "True"
    type: Succeeded
  taskResults:
  - name: aResult
    value: aResultValue
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("test-pipeline-run-final-task-results-dag-task-2-xxyyy", "foo",
				"test-pipeline-run-final-task-results", "test-pipeline", "dag-task-2", false),
			`
spec:
  serviceAccountName: test-sa
  taskRef:
    name: hello-world
status:
  conditions:
  - lastTransitionTime: null
    status: "False"
    type: Succeeded
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		TaskRuns:     trs,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-final-task-results", []string{}, false)

	expectedTaskRunName := "test-pipeline-run-final-task-results-final-task-1"
	expectedTaskRunObjectMeta := taskRunObjectMeta("test-pipeline-run-final-task-results-final-task-1", "foo",
		"test-pipeline-run-final-task-results", "test-pipeline", "final-task-1", true)
	expectedTaskRunObjectMeta.Labels[pipeline.MemberOfLabelKey] = v1beta1.PipelineFinallyTasks
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t, expectedTaskRunObjectMeta, `
spec:
  params:
  - name: finalParam
    value: aResultValue
  resources: {}
  serviceAccountName: test-sa-0
  taskRef:
    name: final-task
    kind: Task
`)

	// Check that the expected TaskRun was created
	actual, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
		LabelSelector: "tekton.dev/pipelineTask=final-task-1,tekton.dev/pipelineRun=test-pipeline-run-final-task-results",
		Limit:         1,
	})

	if err != nil {
		t.Fatalf("Failure to list TaskRun's %s", err)
	}
	if len(actual.Items) != 1 {
		t.Fatalf("Expected 1 TaskRuns got %d", len(actual.Items))
	}
	actualTaskRun := actual.Items[0]
	if d := cmp.Diff(*expectedTaskRun, actualTaskRun, ignoreResourceVersion, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRunName, diff.PrintWantGot(d))
	}
	expectedSkippedTasks := []v1beta1.SkippedTask{{
		Name:   "final-task-2",
		Reason: v1beta1.MissingResultsSkip,
	}, {
		Name:   "final-task-3",
		Reason: v1beta1.WhenExpressionsSkip,
		WhenExpressions: v1beta1.WhenExpressions{{
			Input:    "aResultValue",
			Operator: "notin",
			Values:   []string{"aResultValue"},
		}},
	}, {
		Name:   "final-task-5",
		Reason: v1beta1.MissingResultsSkip,
	}, {
		Name:   "final-task-6",
		Reason: v1beta1.MissingResultsSkip,
	}}

	if d := cmp.Diff(expectedSkippedTasks, reconciledRun.Status.SkippedTasks); d != "" {
		t.Fatalf("Didn't get the expected list of skipped tasks. Diff: %s", diff.PrintWantGot(d))
	}
}

// newPipelineRunTest returns PipelineRunTest with a new PipelineRun controller created with specified state through data
// This PipelineRunTest can be reused for multiple PipelineRuns by calling reconcileRun for each pipelineRun
func newPipelineRunTest(data test.Data, t *testing.T) *PipelineRunTest {
	t.Helper()
	testAssets, cancel := getPipelineRunController(t, data)
	return &PipelineRunTest{
		Data:       data,
		Test:       t,
		TestAssets: testAssets,
		Cancel:     cancel,
	}
}

func (prt PipelineRunTest) reconcileRun(namespace, pipelineRunName string, wantEvents []string, permanentError bool) (*v1beta1.PipelineRun, test.Clients) {
	prt.Test.Helper()
	c := prt.TestAssets.Controller
	clients := prt.TestAssets.Clients
	reconcileError := c.Reconciler.Reconcile(prt.TestAssets.Ctx, namespace+"/"+pipelineRunName)
	if permanentError {
		// When a PipelineRun is invalid and can't run, we expect a permanent error that will
		// tell the Reconciler to not keep trying to reconcile.
		if reconcileError == nil {
			prt.Test.Fatalf("Expected an error to be returned by Reconcile, got nil instead")
		}
		if controller.IsPermanentError(reconcileError) != permanentError {
			prt.Test.Fatalf("Expected the error to be permanent: %v but got: %v", permanentError, controller.IsPermanentError(reconcileError))
		}
	} else if ok, _ := controller.IsRequeueKey(reconcileError); ok {
		// This is normal, it happens for timeouts when we otherwise successfully reconcile.
	} else if reconcileError != nil {
		prt.Test.Fatalf("Error reconciling: %s", reconcileError)
	}
	// Check that the PipelineRun was reconciled correctly
	reconciledRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns(namespace).Get(prt.TestAssets.Ctx, pipelineRunName, metav1.GetOptions{})
	if err != nil {
		prt.Test.Fatalf("Somehow had error getting reconciled run out of fake client: %s", err)
	}

	// Check generated events match what's expected
	if len(wantEvents) > 0 {
		if err := eventstest.CheckEventsOrdered(prt.Test, prt.TestAssets.Recorder.Events, pipelineRunName, wantEvents); err != nil {
			prt.Test.Errorf(err.Error())
		}
	}

	return reconciledRun, clients
}

func TestReconcile_RemotePipelineRef(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileRemotePipelineRef(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileRemotePipelineRef(t *testing.T, embeddedStatus string) {
	names.TestingSeed()

	ctx := context.Background()
	cfg := config.NewStore(logtesting.TestLogger(t))
	cfg.OnConfigChanged(withOCIBundles(newFeatureFlagsConfigMap()))
	ctx = cfg.ToContext(ctx)

	// Set up a fake registry to push an image to.
	s := httptest.NewServer(registry.New())
	defer s.Close()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}

	ref := u.Host + "/testreconcile_remotepipelineref"

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: test-pipeline-run-success
  namespace: foo
spec:
  pipelineRef:
    bundle: %s
    name: test-pipeline
  serviceAccountName: test-sa
  timeout: 1h0m0s
`, ref))}
	ps := parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
  - name: unit-test-1
    taskRef:
      bundle: %s
      name: unit-test-task
`, ref))
	cms := []*corev1.ConfigMap{withOCIBundles(withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus))}

	// This task will be uploaded along with the pipeline definition.
	remoteTask := parse.MustParseTask(t, `
metadata:
  name: unit-test-task
  namespace: foo
`)

	// Create a bundle from our pipeline and tasks.
	if _, err := test.CreateImage(ref, ps, remoteTask); err != nil {
		t.Fatalf("failed to create image in pipeline renconcile: %s", err.Error())
	}

	// Unlike the tests above, we do *not* locally define our pipeline or unit-test task.
	d := test.Data{
		PipelineRuns: prs,
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: prs[0].Spec.ServiceAccountName, Namespace: "foo"},
		}},
		ConfigMaps: cms,
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-success", wantEvents, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-success-unit-test-1", "foo", "test-pipeline-run-success",
			"test-pipeline", "unit-test-1", false),
		fmt.Sprintf(`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    bundle: %s
    kind: Task
    name: unit-test-task
`, ref))

	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta, cmpopts.SortSlices(lessTaskResourceBindings)); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}

	// This PipelineRun is in progress now and the status should reflect that
	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())

	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, "test-pipeline-run-success-unit-test-1")
}

// TestReconcile_OptionalWorkspacesOmitted checks that an optional workspace declared by
// a Task and a Pipeline can be omitted by a PipelineRun and the run will still start
// successfully without an error.
func TestReconcile_OptionalWorkspacesOmitted(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileOptionalWorkspacesOmitted(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileOptionalWorkspacesOmitted(t *testing.T, embeddedStatus string) {
	names.TestingSeed()

	ctx := context.Background()
	cfg := config.NewStore(logtesting.TestLogger(t))
	ctx = cfg.ToContext(ctx)

	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-success
  namespace: foo
spec:
  pipelineSpec:
    tasks:
    - name: unit-test-1
      taskSpec:
        steps:
        - image: foo:latest
        workspaces:
        - name: ws
          optional: true
      workspaces:
      - name: ws
        workspace: optional-workspace
    workspaces:
    - name: optional-workspace
      optional: true
  serviceAccountName: test-sa
`)}

	// Unlike the tests above, we do *not* locally define our pipeline or unit-test task.
	d := test.Data{
		PipelineRuns: prs,
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: prs[0].Spec.ServiceAccountName, Namespace: "foo"},
		}},
		ConfigMaps: []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-success", nil, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-success-unit-test-1", "foo", "test-pipeline-run-success",
			"test-pipeline-run-success", "unit-test-1", false),
		`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskSpec:
    steps:
    - image: foo:latest
    workspaces:
    - name: ws
      optional: true
`)

	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta, cmpopts.SortSlices(lessTaskResourceBindings)); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}

	// This PipelineRun is in progress now and the status should reflect that
	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())

	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 1)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, "test-pipeline-run-success-unit-test-1")
}

func TestReconcile_DependencyValidationsImmediatelyFailPipelineRun(t *testing.T) {
	names.TestingSeed()

	ctx := context.Background()
	cfg := config.NewStore(logtesting.TestLogger(t))
	ctx = cfg.ToContext(ctx)

	prs := []*v1beta1.PipelineRun{
		parse.MustParsePipelineRun(t, `
metadata:
  name: pipelinerun-param-invalid-result-variable
  namespace: foo
spec:
  pipelineSpec:
    tasks:
    - name: pt0
      taskSpec:
        steps:
        - image: foo:latest
    - name: pt1
      params:
      - name: p
        value: $(tasks.pt0.results.r1)
      taskSpec:
        params:
        - name: p
        steps:
        - image: foo:latest
  serviceAccountName: test-sa
`),
		parse.MustParsePipelineRun(t, `
metadata:
  name: pipelinerun-pipeline-result-invalid-result-variable
  namespace: foo
spec:
  pipelineSpec:
    results:
    - name: pr
      value: $(tasks.pt0.results.r)
    tasks:
    - name: pt0
      taskSpec:
        steps:
        - image: foo:latest
    - name: pt1
      taskSpec:
        steps:
        - image: foo:latest
  serviceAccountName: test-sa
`),
		parse.MustParsePipelineRun(t, `
metadata:
  name: pipelinerun-with-optional-workspace-validation
  namespace: foo
spec:
  pipelineSpec:
    tasks:
    - name: unit-test-1
      taskSpec:
        steps:
        - image: foo:latest
        workspaces:
        - name: ws
      workspaces:
      - name: ws
        workspace: optional-workspace
    workspaces:
    - name: optional-workspace
      optional: true
  serviceAccountName: test-sa
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: prs[0].Spec.ServiceAccountName, Namespace: "foo"},
		}},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	run1, _ := prt.reconcileRun("foo", "pipelinerun-param-invalid-result-variable", nil, true)
	run2, _ := prt.reconcileRun("foo", "pipelinerun-pipeline-result-invalid-result-variable", nil, true)
	run3, _ := prt.reconcileRun("foo", "pipelinerun-with-optional-workspace-validation", nil, true)

	cond1 := run1.Status.GetCondition(apis.ConditionSucceeded)
	cond2 := run2.Status.GetCondition(apis.ConditionSucceeded)
	cond3 := run3.Status.GetCondition(apis.ConditionSucceeded)

	for _, c := range []*apis.Condition{cond1, cond2, cond3} {
		if c.Status != corev1.ConditionFalse {
			t.Errorf("expected Succeeded/False condition but saw: %v", c)
		}
	}

	if cond1.Reason != ReasonInvalidTaskResultReference {
		t.Errorf("expected invalid task reference condition but saw: %v", cond1)
	}

	if cond2.Reason != ReasonInvalidTaskResultReference {
		t.Errorf("expected invalid task reference condition but saw: %v", cond2)
	}

	if cond3.Reason != ReasonRequiredWorkspaceMarkedOptional {
		t.Errorf("expected optional workspace not supported condition but saw: %v", cond3)
	}
}

// TestReconcileWithResolver checks that a PipelineRun with a populated Resolver
// field creates a ResolutionRequest object for that Resolver's type, and
// that when the request is successfully resolved the PipelineRun begins running.
func TestReconcileWithResolver(t *testing.T) {
	resolverName := "foobar"
	pr := parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: default
spec:
  pipelineRef:
    resolver: foobar
  serviceAccountName: default
`)

	d := test.Data{
		PipelineRuns: []*v1beta1.PipelineRun{pr},
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: pr.Spec.ServiceAccountName, Namespace: "foo"},
		}},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string(nil)
	pipelinerun, _ := prt.reconcileRun(pr.Namespace, pr.Name, wantEvents, false)
	checkPipelineRunConditionStatusAndReason(t, pipelinerun, corev1.ConditionUnknown, ReasonResolvingPipelineRef)

	client := prt.TestAssets.Clients.ResolutionRequests.ResolutionV1beta1().ResolutionRequests("default")
	resolutionrequests, err := client.List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error listing resource requests: %v", err)
	}
	numResolutionRequests := len(resolutionrequests.Items)
	if numResolutionRequests != 1 {
		t.Fatalf("expected exactly 1 resource request but found %d", numResolutionRequests)
	}

	resreq := &resolutionrequests.Items[0]
	resolutionRequestType := resreq.ObjectMeta.Labels["resolution.tekton.dev/type"]
	if resolutionRequestType != resolverName {
		t.Fatalf("expected resource request type %q but saw %q", resolutionRequestType, resolverName)
	}

	// Mock a successful resolution
	var pipelineBytes = []byte(`
          kind: Pipeline
          apiVersion: tekton.dev/v1beta1
          metadata:
            name: foo
          spec:
            tasks:
            - name: task1
              taskSpec:
                steps:
                - name: step1
                  image: ubuntu
                  script: |
                    echo "hello world!"
        `)
	resreq.Status.ResolutionRequestStatusFields.Data = base64.StdEncoding.Strict().EncodeToString(pipelineBytes)
	resreq.Status.MarkSucceeded()
	resreq, err = client.UpdateStatus(prt.TestAssets.Ctx, resreq, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("unexpected error updating resource request with resolved pipeline data: %v", err)
	}

	// Check that the resolved pipeline was recognized by the
	// PipelineRun reconciler and that the PipelineRun has now
	// started executing.
	updatedPipelineRun, _ := prt.reconcileRun("default", "pr", nil, false)
	checkPipelineRunConditionStatusAndReason(t, updatedPipelineRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())
}

// TestReconcileWithFailingResolver checks that a PipelineRun with a failing Resolver
// field creates a ResolutionRequest object for that Resolver's type, and
// that when the request fails, the PipelineRun fails.
func TestReconcileWithFailingResolver(t *testing.T) {
	resolverName := "does-not-exist"
	pr := parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: default
spec:
  pipelineRef:
    resolver: does-not-exist
  serviceAccountName: default
`)

	d := test.Data{
		PipelineRuns: []*v1beta1.PipelineRun{pr},
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: pr.Spec.ServiceAccountName, Namespace: "foo"},
		}},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string(nil)
	pipelinerun, _ := prt.reconcileRun(pr.Namespace, pr.Name, wantEvents, false)
	checkPipelineRunConditionStatusAndReason(t, pipelinerun, corev1.ConditionUnknown, ReasonResolvingPipelineRef)

	client := prt.TestAssets.Clients.ResolutionRequests.ResolutionV1beta1().ResolutionRequests("default")
	resolutionrequests, err := client.List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error listing resource requests: %v", err)
	}
	numResolutionRequests := len(resolutionrequests.Items)
	if numResolutionRequests != 1 {
		t.Fatalf("expected exactly 1 resource request but found %d", numResolutionRequests)
	}

	resreq := &resolutionrequests.Items[0]
	resolutionRequestType := resreq.ObjectMeta.Labels["resolution.tekton.dev/type"]
	if resolutionRequestType != resolverName {
		t.Fatalf("expected resource request type %q but saw %q", resolutionRequestType, resolverName)
	}

	resreq.Status.MarkFailed(resolutioncommon.ReasonResolutionTimedOut, "resolution took longer than global timeout of 1 minute")
	resreq, err = client.UpdateStatus(prt.TestAssets.Ctx, resreq, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("unexpected error updating resource request with resolved pipeline data: %v", err)
	}

	// Check that the pipeline fails with the appropriate reason.
	updatedPipelineRun, _ := prt.reconcileRun("default", "pr", nil, true)
	checkPipelineRunConditionStatusAndReason(t, updatedPipelineRun, corev1.ConditionFalse, ReasonCouldntGetPipeline)
}

// TestReconcileWithFailingTaskResolver checks that a PipelineRun with a failing Resolver
// field for a Task creates a ResolutionRequest object for that Resolver's type, and
// that when the request fails, the PipelineRun fails.
func TestReconcileWithFailingTaskResolver(t *testing.T) {
	resolverName := "foobar"
	pr := parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: default
spec:
  pipelineSpec:
    tasks:
    - name: some-task
      taskRef:
        resolver: foobar
  serviceAccountName: default
`)

	d := test.Data{
		PipelineRuns: []*v1beta1.PipelineRun{pr},
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: pr.Spec.ServiceAccountName, Namespace: "foo"},
		}},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string(nil)
	pipelinerun, _ := prt.reconcileRun(pr.Namespace, pr.Name, wantEvents, false)
	checkPipelineRunConditionStatusAndReason(t, pipelinerun, corev1.ConditionUnknown, v1beta1.TaskRunReasonResolvingTaskRef)

	client := prt.TestAssets.Clients.ResolutionRequests.ResolutionV1beta1().ResolutionRequests("default")
	resolutionrequests, err := client.List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error listing resource requests: %v", err)
	}
	numResolutionRequests := len(resolutionrequests.Items)
	if numResolutionRequests != 1 {
		t.Fatalf("expected exactly 1 resource request but found %d", numResolutionRequests)
	}

	resreq := &resolutionrequests.Items[0]
	resolutionRequestType := resreq.ObjectMeta.Labels["resolution.tekton.dev/type"]
	if resolutionRequestType != resolverName {
		t.Fatalf("expected resource request type %q but saw %q", resolutionRequestType, resolverName)
	}

	resreq.Status.MarkFailed(resolutioncommon.ReasonResolutionTimedOut, "resolution took longer than global timeout of 1 minute")
	resreq, err = client.UpdateStatus(prt.TestAssets.Ctx, resreq, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("unexpected error updating resource request with resolved pipeline data: %v", err)
	}

	// Check that the pipeline fails.
	updatedPipelineRun, _ := prt.reconcileRun("default", "pr", nil, true)
	checkPipelineRunConditionStatusAndReason(t, updatedPipelineRun, corev1.ConditionFalse, ReasonCouldntGetTask)
}

// TestReconcileWithTaskResolver checks that a PipelineRun with a populated Resolver
// field for a Task creates a ResolutionRequest object for that Resolver's type, and
// that when the request is successfully resolved the PipelineRun begins running.
func TestReconcileWithTaskResolver(t *testing.T) {
	resolverName := "foobar"
	pr := parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: default
spec:
  pipelineSpec:
    tasks:
    - name: some-task
      taskRef:
        resolver: foobar
        params:
        - name: foo
          value: bar
  serviceAccountName: default
`)

	d := test.Data{
		PipelineRuns: []*v1beta1.PipelineRun{pr},
		ServiceAccounts: []*corev1.ServiceAccount{{
			ObjectMeta: metav1.ObjectMeta{Name: pr.Spec.ServiceAccountName, Namespace: "foo"},
		}},
	}

	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string(nil)
	pipelinerun, _ := prt.reconcileRun(pr.Namespace, pr.Name, wantEvents, false)
	checkPipelineRunConditionStatusAndReason(t, pipelinerun, corev1.ConditionUnknown, v1beta1.TaskRunReasonResolvingTaskRef)

	client := prt.TestAssets.Clients.ResolutionRequests.ResolutionV1beta1().ResolutionRequests("default")
	resolutionrequests, err := client.List(prt.TestAssets.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error listing resource requests: %v", err)
	}
	numResolutionRequests := len(resolutionrequests.Items)
	if numResolutionRequests != 1 {
		t.Fatalf("expected exactly 1 resource request but found %d", numResolutionRequests)
	}

	resreq := &resolutionrequests.Items[0]
	resolutionRequestType := resreq.ObjectMeta.Labels["resolution.tekton.dev/type"]
	if resolutionRequestType != resolverName {
		t.Fatalf("expected resource request type %q but saw %q", resolutionRequestType, resolverName)
	}
	fooVal := ""
	for _, p := range resreq.Spec.Params {
		if p.Name == "foo" {
			fooVal = p.Value.StringVal
		}
	}
	if fooVal != "bar" {
		t.Fatalf("expected resource request parameter 'bar', but got '%s'", fooVal)
	}

	taskBytes := []byte(`
kind: Task
apiVersion: tekton.dev/v1beta1
metadata:
  name: foo
spec:
  steps:
  - name: step1
    image: ubuntu
    script: |
      echo "hello world!"
`)

	resreq.Status.ResolutionRequestStatusFields.Data = base64.StdEncoding.Strict().EncodeToString(taskBytes)
	resreq.Status.MarkSucceeded()
	resreq, err = client.UpdateStatus(prt.TestAssets.Ctx, resreq, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("unexpected error updating resource request with resolved pipeline data: %v", err)
	}

	// Check that the resolved pipeline was recognized by the
	// PipelineRun reconciler and that the PipelineRun has now
	// started executing.
	updatedPipelineRun, _ := prt.reconcileRun("default", "pr", nil, false)
	checkPipelineRunConditionStatusAndReason(t, updatedPipelineRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())
}

func getTaskRunWithTaskSpec(tr, pr, p, t string, labels, annotations map[string]string) *v1beta1.TaskRun {
	om := taskRunObjectMeta(tr, "foo", pr, p, t, false)
	for k, v := range labels {
		om.Labels[k] = v
	}
	om.Annotations = annotations

	return &v1beta1.TaskRun{
		ObjectMeta: om,
		Spec: v1beta1.TaskRunSpec{
			TaskSpec: &v1beta1.TaskSpec{
				Steps: []v1beta1.Step{{
					Name:  "mystep",
					Image: "myimage",
				}},
			},
			ServiceAccountName: config.DefaultServiceAccountValue,
			Resources:          &v1beta1.TaskRunResources{},
		},
	}
}

func baseObjectMeta(name, ns string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   ns,
		Labels:      map[string]string{},
		Annotations: map[string]string{},
	}
}

func taskRunObjectMeta(trName, ns, prName, pipelineName, pipelineTaskName string, skipMemberOfLabel bool) metav1.ObjectMeta {
	om := metav1.ObjectMeta{
		Name:      trName,
		Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{
			Kind:               "PipelineRun",
			Name:               prName,
			APIVersion:         "tekton.dev/v1beta1",
			Controller:         &trueb,
			BlockOwnerDeletion: &trueb,
		}},
		Labels: map[string]string{
			pipeline.PipelineLabelKey:     pipelineName,
			pipeline.PipelineRunLabelKey:  prName,
			pipeline.PipelineTaskLabelKey: pipelineTaskName,
		},
		Annotations: map[string]string{},
	}
	if !skipMemberOfLabel {
		om.Labels[pipeline.MemberOfLabelKey] = v1beta1.PipelineTasks
	}
	return om
}

func taskRunObjectMetaWithAnnotations(trName, ns, prName, pipelineName, pipelineTaskName string, skipMemberOfLabel bool, annotations map[string]string) metav1.ObjectMeta {
	om := taskRunObjectMeta(trName, ns, prName, pipelineName, pipelineTaskName, skipMemberOfLabel)
	for k, v := range annotations {
		om.Annotations[k] = v
	}
	return om
}

func createHelloWorldTaskRunWithStatus(
	t *testing.T,
	trName, ns, prName, pName, podName string,
	condition apis.Condition,
) *v1beta1.TaskRun {
	p := createHelloWorldTaskRun(t, trName, ns, prName, pName)
	p.Status = v1beta1.TaskRunStatus{
		Status: duckv1beta1.Status{
			Conditions: duckv1beta1.Conditions{condition},
		},
		TaskRunStatusFields: v1beta1.TaskRunStatusFields{
			PodName: podName,
		},
	}
	return p
}

func createHelloWorldTaskRunWithStatusTaskLabel(
	t *testing.T,
	trName, ns, prName, pName, podName, taskLabel string,
	condition apis.Condition,
) *v1beta1.TaskRun {
	p := createHelloWorldTaskRunWithStatus(t, trName, ns, prName, pName, podName, condition)
	p.Labels[pipeline.PipelineTaskLabelKey] = taskLabel

	return p
}

func createHelloWorldTaskRun(t *testing.T, trName, ns, prName, pName string) *v1beta1.TaskRun {
	return parse.MustParseTaskRun(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    %s: %s
spec:
  taskRef:
    name: hello-world
  serviceAccountName: test-sa
`, trName, ns, pipeline.PipelineLabelKey, pName, pipeline.PipelineRunLabelKey, prName))
}

func createCancelledPipelineRun(t *testing.T, prName string, specStatus v1beta1.PipelineRunSpecStatus) *v1beta1.PipelineRun {
	return parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
  status: %s
status:
  startTime: %s`, prName, specStatus, now.Format(time.RFC3339)))
}

func shouldHaveFullEmbeddedStatus(embeddedVal string) bool {
	return embeddedVal == config.FullEmbeddedStatus || embeddedVal == config.BothEmbeddedStatus
}

func shouldHaveMinimalEmbeddedStatus(embeddedVal string) bool {
	return embeddedVal == config.MinimalEmbeddedStatus || embeddedVal == config.BothEmbeddedStatus
}

func verifyTaskRunStatusesCount(t *testing.T, embeddedStatus string, prStatus v1beta1.PipelineRunStatus, taskCount int) {
	t.Helper()
	if shouldHaveFullEmbeddedStatus(embeddedStatus) && len(prStatus.TaskRuns) != taskCount {
		t.Errorf("Expected PipelineRun status to have exactly %d tasks, but was %d", taskCount, len(prStatus.TaskRuns))
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) && len(filterChildRefsForKind(prStatus.ChildReferences, "TaskRun")) != taskCount {
		t.Errorf("Expected PipelineRun status ChildReferences to have %d tasks, but was %d", taskCount, len(filterChildRefsForKind(prStatus.ChildReferences, "TaskRun")))
	}
}

func verifyTaskRunStatusesNames(t *testing.T, embeddedStatus string, prStatus v1beta1.PipelineRunStatus, taskNames ...string) {
	t.Helper()
	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		for _, tn := range taskNames {
			if _, ok := prStatus.TaskRuns[tn]; !ok {
				t.Errorf("Expected PipelineRun status to include TaskRun status for %s but was %v", tn, prStatus.TaskRuns)
			}
		}
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		tnMap := make(map[string]struct{})
		for _, cr := range filterChildRefsForKind(prStatus.ChildReferences, "TaskRun") {
			tnMap[cr.Name] = struct{}{}
		}

		for _, tn := range taskNames {
			if _, ok := tnMap[tn]; !ok {
				t.Errorf("Expected PipelineRun status to include TaskRun status for %s but was %v", tn, prStatus.ChildReferences)
			}
		}
	}
}

func verifyRunStatusesCount(t *testing.T, embeddedStatus string, prStatus v1beta1.PipelineRunStatus, runCount int) {
	t.Helper()
	if shouldHaveFullEmbeddedStatus(embeddedStatus) && len(prStatus.Runs) != runCount {
		t.Errorf("Expected PipelineRun status to have exactly %d runs, but was %d", runCount, len(prStatus.Runs))
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) && len(filterChildRefsForKind(prStatus.ChildReferences, "Run")) != runCount {
		t.Errorf("Expected PipelineRun status ChildReferences to have %d runs, but was %d", runCount, len(filterChildRefsForKind(prStatus.ChildReferences, "Run")))
	}
}

func verifyRunStatusesNames(t *testing.T, embeddedStatus string, prStatus v1beta1.PipelineRunStatus, runNames ...string) {
	t.Helper()
	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		for _, rn := range runNames {
			if _, ok := prStatus.Runs[rn]; !ok {
				t.Errorf("Expected PipelineRun status to include Run status for %s but was %v", rn, prStatus.Runs)
			}
		}
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		rnMap := make(map[string]struct{})
		for _, cr := range filterChildRefsForKind(prStatus.ChildReferences, "Run") {
			rnMap[cr.Name] = struct{}{}
		}

		for _, rn := range runNames {
			if _, ok := rnMap[rn]; !ok {
				t.Errorf("Expected PipelineRun status to include Run status for %s but was %v", rn, prStatus.ChildReferences)
			}
		}
	}
}

func verifyTaskRunStatusesWhenExpressions(t *testing.T, embeddedStatus string, prStatus v1beta1.PipelineRunStatus, trName string, expectedWhen []v1beta1.WhenExpression) {
	t.Helper()
	if shouldHaveFullEmbeddedStatus(embeddedStatus) {
		actualWhenExpressionsInTaskRun := prStatus.TaskRuns[trName].WhenExpressions
		if d := cmp.Diff(expectedWhen, actualWhenExpressionsInTaskRun); d != "" {
			t.Errorf("expected to see When Expressions %v created. Diff %s", trName, diff.PrintWantGot(d))
		}
	}
	if shouldHaveMinimalEmbeddedStatus(embeddedStatus) {
		var actualWhenExpressionsInTaskRun []v1beta1.WhenExpression
		for _, cr := range prStatus.ChildReferences {
			if cr.Name == trName {
				actualWhenExpressionsInTaskRun = append(actualWhenExpressionsInTaskRun, cr.WhenExpressions...)
			}
		}
		if d := cmp.Diff(expectedWhen, actualWhenExpressionsInTaskRun); d != "" {
			t.Errorf("expected to see When Expressions %v created. Diff %s", trName, diff.PrintWantGot(d))
		}
	}
}

func filterChildRefsForKind(childRefs []v1beta1.ChildStatusReference, kind string) []v1beta1.ChildStatusReference {
	var filtered []v1beta1.ChildStatusReference
	for _, cr := range childRefs {
		if cr.Kind == kind {
			filtered = append(filtered, cr)
		}
	}
	return filtered
}

func mustParseTaskRunWithObjectMeta(t *testing.T, objectMeta metav1.ObjectMeta, asYAML string) *v1beta1.TaskRun {
	tr := parse.MustParseTaskRun(t, asYAML)
	tr.ObjectMeta = objectMeta
	return tr
}

func mustParseRunWithObjectMeta(t *testing.T, objectMeta metav1.ObjectMeta, asYAML string) *v1alpha1.Run {
	r := parse.MustParseRun(t, asYAML)
	r.ObjectMeta = objectMeta
	return r
}

func helloWorldPipelineWithRunAfter(t *testing.T) *v1beta1.Pipeline {
	return parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
    - name: hello-world-1
      taskRef:
        name: hello-world
    - name: hello-world-2
      taskRef:
        name: hello-world
      runAfter:
        - hello-world-1
`)
}

func checkPipelineRunConditionStatusAndReason(t *testing.T, reconciledRun *v1beta1.PipelineRun, conditionStatus corev1.ConditionStatus, conditionReason string) {
	t.Helper()

	condition := reconciledRun.Status.GetCondition(apis.ConditionSucceeded)
	if condition == nil || condition.Status != conditionStatus {
		t.Errorf("Expected PipelineRun status to be %s, but was %v", conditionStatus, condition)
	}
	if condition != nil && condition.Reason != conditionReason {
		t.Errorf("Expected reason %s but was %s", conditionReason, condition.Reason)
	}
}

func TestGetTaskrunWorkspaces_Failure(t *testing.T) {
	tests := []struct {
		name          string
		pr            *v1beta1.PipelineRun
		expectedError string
	}{{
		name: "failure declaring workspace with different name",
		pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source
  pipelineSpec:
    workspaces:
      - name: source
    tasks:
      - name: resolved-pipelinetask
        workspaces:
          - name: not-source
`),
		expectedError: `expected workspace "not-source" to be provided by pipelinerun for pipeline task "resolved-pipelinetask"`,
	},
		{
			name: "failure mapping workspace with different name",
			pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source
  pipelineSpec:
    workspaces:
      - name: source
    tasks:
      - name: resolved-pipelinetask
        workspaces:
          - name: not-source
            workspace: ""
`),
			expectedError: `expected workspace "not-source" to be provided by pipelinerun for pipeline task "resolved-pipelinetask"`,
		},
		{
			name: "failure propagating workspaces using scripts",
			pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source
  pipelineSpec:
    workspaces:
      - name: source
    tasks:
      - name: resolved-pipelinetask
        taskSpec:
          steps:
            - name: mystep
              image: myimage
              script: echo $(workspaces.not-source.path)
`),
			expectedError: `expected workspace "not-source" to be provided by pipelinerun for pipeline task "resolved-pipelinetask"`,
		},
		{
			name: "failure propagating workspaces using args",
			pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source
  pipelineSpec:
    workspaces:
      - name: source
    tasks:
      - name: resolved-pipelinetask
        taskSpec:
          steps:
            - name: mystep
              image: myimage
              command:
                - /mycmd
              args:
                - echo $(workspaces.not-source.path)
        workspaces:
          - name: source
`),
			expectedError: `expected workspace "not-source" to be provided by pipelinerun for pipeline task "resolved-pipelinetask"`,
		},
	}
	ctx := config.EnableAlphaAPIFields(context.Background())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rprt := &resources.ResolvedPipelineTask{PipelineTask: &tt.pr.Spec.PipelineSpec.Tasks[0]}
			_, _, err := getTaskrunWorkspaces(ctx, tt.pr, rprt)
			if err == nil {
				t.Errorf("Pipeline.getTaskrunWorkspaces() did not return error for invalid workspace")
			} else if d := cmp.Diff(tt.expectedError, err.Error(), cmpopts.IgnoreUnexported(apis.FieldError{})); d != "" {
				t.Errorf("Pipeline.getTaskrunWorkspaces() errors diff %s", diff.PrintWantGot(d))
			}
		})
	}

}

func TestGetTaskrunWorkspaces_Success(t *testing.T) {
	tests := []struct {
		name string
		pr   *v1beta1.PipelineRun
		rprt *resources.ResolvedPipelineTask
	}{{
		name: "valid declaration of workspace names",
		pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source`),
		rprt: &resources.ResolvedPipelineTask{
			PipelineTask: &v1beta1.PipelineTask{
				Name: "resolved-pipelinetask",
				Workspaces: []v1beta1.WorkspacePipelineTaskBinding{{
					Name:      "my-task-workspace",
					Workspace: "source",
				}},
			},
		},
	},
		{
			name: "valid mapping with same workspace names",
			pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source`),
			rprt: &resources.ResolvedPipelineTask{
				PipelineTask: &v1beta1.PipelineTask{
					Name: "resolved-pipelinetask",
					Workspaces: []v1beta1.WorkspacePipelineTaskBinding{{
						Name:      "source",
						Workspace: "",
					}},
				},
			},
		},
		{
			name: "propagating workspaces using scripts",
			pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source`),
			rprt: &resources.ResolvedPipelineTask{
				PipelineTask: &v1beta1.PipelineTask{
					Name: "resolved-pipelinetask",
					TaskSpec: &v1beta1.EmbeddedTask{
						TaskSpec: v1beta1.TaskSpec{
							Steps: []v1beta1.Step{{
								Name:   "mystep",
								Image:  "myimage",
								Script: "echo $(workspaces.source.path)",
							}},
						},
					},
				},
			},
		},
		{
			name: "propagating workspaces using args",
			pr: parse.MustParsePipelineRun(t, `
metadata:
  name: pipeline
spec:
  workspaces:
    - name: source`),
			rprt: &resources.ResolvedPipelineTask{
				PipelineTask: &v1beta1.PipelineTask{
					Name: "resolved-pipelinetask",
					TaskSpec: &v1beta1.EmbeddedTask{
						TaskSpec: v1beta1.TaskSpec{
							Steps: []v1beta1.Step{{
								Name:    "mystep",
								Image:   "myimage",
								Command: []string{"/mycmd"},
								Args:    []string{"echo $(workspaces.source.path)"},
							}},
						},
					},
					Workspaces: []v1beta1.WorkspacePipelineTaskBinding{{
						Name: "source",
					}},
				},
			},
		},
	}
	ctx := config.EnableAlphaAPIFields(context.Background())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := getTaskrunWorkspaces(ctx, tt.pr, tt.rprt)

			if err != nil {
				t.Errorf("Pipeline.getTaskrunWorkspaces() returned error for valid pipeline: %v", err)
			}
		})
	}
}

func TestReconcile_PropagatePipelineTaskRunSpecMetadata(t *testing.T) {
	names.TestingSeed()
	prName := "test-pipeline-run"
	ps := []*v1beta1.Pipeline{simpleHelloWorldPipeline}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
spec:
  pipelineRef:
    name: test-pipeline
  taskRunSpecs:
  - pipelineTaskName: hello-world-1
    metadata:
      labels:
        PipelineTaskRunSpecLabel: PipelineTaskRunSpecValue
      annotations:
        PipelineTaskRunSpecAnnotation: PipelineTaskRunSpecValue
    taskServiceAccountName: custom-sa
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", prName, []string{}, false)

	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRunObjectMeta := taskRunObjectMeta("test-pipeline-run-hello-world-1", "foo", "test-pipeline-run", "test-pipeline", "hello-world-1", false)
	expectedTaskRunObjectMeta.Labels["PipelineTaskRunSpecLabel"] = "PipelineTaskRunSpecValue"
	expectedTaskRunObjectMeta.Annotations["PipelineTaskRunSpecAnnotation"] = "PipelineTaskRunSpecValue"
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t, expectedTaskRunObjectMeta, `
spec:
  resources: {}
  serviceAccountName: custom-sa
  taskRef:
    name: hello-world
    kind: Task
`)

	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see propagated metadata from PipelineTaskRunSpec in TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}
}

func TestReconcile_AddMetadataByPrecedence(t *testing.T) {
	names.TestingSeed()
	prName := "test-pipeline-run"
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  tasks:
    - name: hello-world-1
      taskSpec:
        steps:
          - name: foo-step
            image: foo-image
        metadata:
          labels:
            TestPrecedenceLabel: PipelineTaskSpecValue
          annotations:
            TestPrecedenceAnnotation: PipelineTaskSpecValue
`)}
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run
  namespace: foo
  metadata:
    labels:
      TestPrecedenceLabel: PipelineRunValue
    annotations:
      TestPrecedenceAnnotation: PipelineRunValue
spec:
  pipelineRef:
    name: test-pipeline
  taskRunSpecs:
  - pipelineTaskName: hello-world-1
    metadata:
      labels:
        TestPrecedenceLabel: PipelineTaskRunSpecValue
      annotations:
        TestPrecedenceAnnotation: PipelineTaskRunSpecValue
    taskServiceAccountName: custom-sa
`)}
	ts := []*v1beta1.Task{simpleHelloWorldTask}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	_, clients := prt.reconcileRun("foo", prName, []string{}, false)

	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRunObjectMeta := taskRunObjectMeta("test-pipeline-run-hello-world-1", "foo", "test-pipeline-run", "test-pipeline", "hello-world-1", false)
	expectedTaskRunObjectMeta.Labels["TestPrecedenceLabel"] = "PipelineTaskRunSpecValue"
	expectedTaskRunObjectMeta.Annotations["TestPrecedenceAnnotation"] = "PipelineTaskRunSpecValue"
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t, expectedTaskRunObjectMeta, `
spec:
  resources: {}
  serviceAccountName: custom-sa
  taskSpec:
    steps:
      - name: foo-step
        image: foo-image
`)

	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta); d != "" {
		t.Errorf("expected to see propagated metadata by the precedence from PipelineTaskRunSpec in TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}
}

func TestReconciler_PipelineTaskMatrix(t *testing.T) {
	names.TestingSeed()

	task := parse.MustParseTask(t, `
metadata:
  name: mytask
  namespace: foo
spec:
  params:
    - name: platform
    - name: browser
    - name: version
  steps:
    - name: echo
      image: alpine
      script: |
        echo "$(params.platform) and $(params.browser) and $(params.version)"
`)

	expectedTaskRuns := []*v1beta1.TaskRun{
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-2", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: windows
  - name: browser
    value: chrome
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-3", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: safari
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-4", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: safari
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-5", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: windows
  - name: browser
    value: safari
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-6", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: firefox
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-7", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: firefox
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-8", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: windows
  - name: browser
    value: firefox
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
	}
	cms := []*corev1.ConfigMap{withEmbeddedStatus(withEnabledAlphaAPIFields(newFeatureFlagsConfigMap()), config.MinimalEmbeddedStatus)}
	cms = append(cms, withMaxMatrixCombinationsCount(newDefaultsConfigMap(), 10))

	tests := []struct {
		name                string
		memberOf            string
		p                   *v1beta1.Pipeline
		tr                  *v1beta1.TaskRun
		expectedPipelineRun *v1beta1.PipelineRun
	}{{
		name:     "p-dag",
		memberOf: "tasks",
		p: parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  tasks:
    - name: platforms-and-browsers
      taskRef:
        name: mytask
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.33.0
`, "p-dag")),
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p-dag
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p-dag
status:
  pipelineSpec:
    tasks:
    - name: platforms-and-browsers
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.33.0
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-2
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-3
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-4
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-5
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-6
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-7
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-8
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
	}, {
		name:     "p-finally",
		memberOf: "finally",
		p: parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  tasks:
    - name: unmatrixed-pt
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.22.0
      taskRef:
        name: mytask
  finally:
    - name: platforms-and-browsers
      taskRef:
        name: mytask
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.33.0
`, "p-finally")),
		tr: mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-unmatrixed-pt", "foo",
				"pr", "p-finally", "unmatrixed-pt", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  - name: version
    value: v0.22.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
status:
 conditions:
  - type: Succeeded
    status: "True"
    reason: Succeeded
    message: All Tasks have completed executing
`),
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p-finally
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p-finally
status:
  pipelineSpec:
    tasks:
    - name: unmatrixed-pt
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.22.0
      taskRef:
        name: mytask
        kind: Task
    finally:
    - name: platforms-and-browsers
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.33.0
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 1 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-unmatrixed-pt
    pipelineTaskName: unmatrixed-pt
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-2
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-3
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-4
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-5
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-6
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-7
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-8
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: pr
  namespace: foo
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: %s
`, tt.name))
			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{pr},
				Pipelines:    []*v1beta1.Pipeline{tt.p},
				Tasks:        []*v1beta1.Task{task},
				ConfigMaps:   cms,
			}
			if tt.tr != nil {
				d.TaskRuns = []*v1beta1.TaskRun{tt.tr}
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			_, clients := prt.reconcileRun("foo", "pr", []string{}, false)
			taskRuns, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("tekton.dev/pipelineRun=pr,tekton.dev/pipeline=%s,tekton.dev/pipelineTask=platforms-and-browsers", tt.name),
				Limit:         1,
			})
			if err != nil {
				t.Fatalf("Failure to list TaskRun's %s", err)
			}

			if len(taskRuns.Items) != 9 {
				t.Fatalf("Expected 9 TaskRuns got %d", len(taskRuns.Items))
			}

			for i := range taskRuns.Items {
				expectedTaskRun := expectedTaskRuns[i]
				expectedTaskRun.Labels["tekton.dev/pipeline"] = tt.name
				expectedTaskRun.Labels["tekton.dev/memberOf"] = tt.memberOf
				if d := cmp.Diff(expectedTaskRun, &taskRuns.Items[i], ignoreResourceVersion, ignoreTypeMeta); d != "" {
					t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRuns[i].Name, diff.PrintWantGot(d))
				}
			}

			pipelineRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(prt.TestAssets.Ctx, "pr", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Got an error getting reconciled run out of fake client: %s", err)
			}
			if d := cmp.Diff(tt.expectedPipelineRun, pipelineRun, ignoreResourceVersion, ignoreTypeMeta, ignoreLastTransitionTime, ignoreStartTime, ignoreFinallyStartTime); d != "" {
				t.Errorf("expected PipelineRun was not created. Diff %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestReconciler_PipelineTaskMatrixWithResults(t *testing.T) {
	names.TestingSeed()

	task := parse.MustParseTask(t, `
metadata:
  name: mytask
  namespace: foo
spec:
  params:
    - name: platform
    - name: browser
    - name: version
  steps:
    - name: echo
      image: alpine
      script: |
        echo "$(params.platform) and $(params.browser) and $(params.version)"
`)

	taskwithresults := parse.MustParseTask(t, `
metadata:
  name: taskwithresults
  namespace: foo
spec:
  results:
   - name: platform-1
   - name: platform-2
   - name: platform-3
   - name: browser-1
   - name: browser-2
   - name: browser-3
   - name: version
  steps:
    - name: echo
      image: alpine
      script: |
        printf linux | tee /tekton/results/platform-1
        printf mac | tee /tekton/results/platform-2
        printf windows | tee /tekton/results/platform-3
        printf chrome | tee /tekton/results/browser-1
        printf safari | tee /tekton/results/browser-2
        printf firefox | tee /tekton/results/browser-3
        printf v0.33.0 | tee /tekton/results/version
`)

	expectedTaskRuns := []*v1beta1.TaskRun{
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-2", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: windows
  - name: browser
    value: chrome
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-3", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: safari
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-4", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: safari
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-5", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: windows
  - name: browser
    value: safari
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-6", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: firefox
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-7", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: firefox
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
		mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-8", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  params:
  - name: platform
    value: windows
  - name: browser
    value: firefox
  - name: version
    value: v0.33.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
    kind: Task
`),
	}
	cms := []*corev1.ConfigMap{withEmbeddedStatus(withEnabledAlphaAPIFields(newFeatureFlagsConfigMap()), config.MinimalEmbeddedStatus)}
	cms = append(cms, withMaxMatrixCombinationsCount(newDefaultsConfigMap(), 10))

	tests := []struct {
		name                string
		memberOf            string
		p                   *v1beta1.Pipeline
		tr                  *v1beta1.TaskRun
		expectedPipelineRun *v1beta1.PipelineRun
	}{{
		name:     "p-dag",
		memberOf: "tasks",
		p: parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  tasks:
    - name: pt-with-result
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.22.0
      taskRef:
        name: taskwithresults
        kind: Task
    - name: platforms-and-browsers
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - $(tasks.pt-with-result.results.platform-1)
              - $(tasks.pt-with-result.results.platform-2)
              - $(tasks.pt-with-result.results.platform-3)
          - name: browser
            value:
              - $(tasks.pt-with-result.results.browser-1)
              - $(tasks.pt-with-result.results.browser-2)
              - $(tasks.pt-with-result.results.browser-3)
      params:
        - name: version
          value: $(tasks.pt-with-result.results.version)
`, "p-dag")),
		tr: mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-pt-with-result", "foo",
				"pr", "p-dag", "pt-with-result", false),
			`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: taskwithresults
  timeout: 1h0m0s
status:
 conditions:
  - type: Succeeded
    status: "True"
    reason: Succeeded
    message: All Tasks have completed executing
 taskResults:
  - name: platform-1
    value: linux
  - name: platform-2
    value: mac
  - name: platform-3
    value: windows
  - name: browser-1
    value: chrome
  - name: browser-2
    value: safari
  - name: browser-3
    value: firefox
  - name: version
    value: v0.33.0
`),
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p-dag
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p-dag
status:
  pipelineSpec:
    tasks:
    - name: pt-with-result
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.22.0
      taskRef:
        name: taskwithresults
        kind: Task
    - name: platforms-and-browsers
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - $(tasks.pt-with-result.results.platform-1)
              - $(tasks.pt-with-result.results.platform-2)
              - $(tasks.pt-with-result.results.platform-3)
          - name: browser
            value:
              - $(tasks.pt-with-result.results.browser-1)
              - $(tasks.pt-with-result.results.browser-2)
              - $(tasks.pt-with-result.results.browser-3)
      params:
        - name: version
          value: $(tasks.pt-with-result.results.version)
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 1 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-pt-with-result
    pipelineTaskName: pt-with-result
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-2
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-3
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-4
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-5
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-6
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-7
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-8
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
	}, {
		name:     "p-finally",
		memberOf: "finally",
		p: parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  tasks:
    - name: pt-with-result
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.22.0
      taskRef:
        name: taskwithresults
        kind: Task
  finally:
    - name: platforms-and-browsers
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - $(tasks.pt-with-result.results.platform-1)
              - $(tasks.pt-with-result.results.platform-2)
              - $(tasks.pt-with-result.results.platform-3)
          - name: browser
            value:
              - $(tasks.pt-with-result.results.browser-1)
              - $(tasks.pt-with-result.results.browser-2)
              - $(tasks.pt-with-result.results.browser-3)
      params:
        - name: version
          value: $(tasks.pt-with-result.results.version)
`, "p-finally")),
		tr: mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-pt-with-result", "foo",
				"pr", "p-finally", "pt-with-result", false),
			`
spec:
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: taskwithresults
    kind: Task
  timeout: 1h0m0s
status:
 conditions:
  - type: Succeeded
    status: "True"
    reason: Succeeded
    message: All Tasks have completed executing
 taskResults:
  - name: platform-1
    value: linux
  - name: platform-2
    value: mac
  - name: platform-3
    value: windows
  - name: browser-1
    value: chrome
  - name: browser-2
    value: safari
  - name: browser-3
    value: firefox
  - name: version
    value: v0.33.0
`),
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p-finally
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p-finally
status:
  pipelineSpec:
    tasks:
    - name: pt-with-result
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.22.0
      taskRef:
        name: taskwithresults
        kind: Task
    finally:
    - name: platforms-and-browsers
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - $(tasks.pt-with-result.results.platform-1)
              - $(tasks.pt-with-result.results.platform-2)
              - $(tasks.pt-with-result.results.platform-3)
          - name: browser
            value:
              - $(tasks.pt-with-result.results.browser-1)
              - $(tasks.pt-with-result.results.browser-2)
              - $(tasks.pt-with-result.results.browser-3)
      params:
        - name: version
          value: $(tasks.pt-with-result.results.version)
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 1 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-pt-with-result
    pipelineTaskName: pt-with-result
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-2
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-3
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-4
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-5
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-6
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-7
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-8
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: pr
  namespace: foo
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: %s
`, tt.name))
			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{pr},
				Pipelines:    []*v1beta1.Pipeline{tt.p},
				Tasks:        []*v1beta1.Task{task, taskwithresults},
				ConfigMaps:   cms,
			}
			if tt.tr != nil {
				d.TaskRuns = []*v1beta1.TaskRun{tt.tr}
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			_, clients := prt.reconcileRun("foo", "pr", []string{}, false)
			taskRuns, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("tekton.dev/pipelineRun=pr,tekton.dev/pipeline=%s,tekton.dev/pipelineTask=platforms-and-browsers", tt.name),
				Limit:         1,
			})
			if err != nil {
				t.Fatalf("Failure to list TaskRun's %s", err)
			}

			if len(taskRuns.Items) != 9 {
				t.Fatalf("Expected 9 TaskRuns got %d", len(taskRuns.Items))
			}

			for i := range taskRuns.Items {
				expectedTaskRun := expectedTaskRuns[i]
				expectedTaskRun.Labels["tekton.dev/pipeline"] = tt.name
				expectedTaskRun.Labels["tekton.dev/memberOf"] = tt.memberOf
				if d := cmp.Diff(expectedTaskRun, &taskRuns.Items[i], ignoreResourceVersion, ignoreTypeMeta); d != "" {
					t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRuns[i].Name, diff.PrintWantGot(d))
				}
			}

			pipelineRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(prt.TestAssets.Ctx, "pr", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Got an error getting reconciled run out of fake client: %s", err)
			}
			if d := cmp.Diff(tt.expectedPipelineRun, pipelineRun, ignoreResourceVersion, ignoreTypeMeta, ignoreLastTransitionTime, ignoreStartTime, ignoreFinallyStartTime); d != "" {
				t.Errorf("expected PipelineRun was not created. Diff %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestReconciler_PipelineTaskMatrixWithRetries(t *testing.T) {
	names.TestingSeed()

	task := parse.MustParseTask(t, `
metadata:
  name: mytask
  namespace: foo
spec:
  params:
    - name: platform
    - name: browser
  steps:
    - name: echo
      image: alpine
      script: |
        echo "$(params.platform) and $(params.browser)"
        exit 1
`)

	cms := []*corev1.ConfigMap{withEmbeddedStatus(withEnabledAlphaAPIFields(newFeatureFlagsConfigMap()), config.MinimalEmbeddedStatus)}
	cms = append(cms, withMaxMatrixCombinationsCount(newDefaultsConfigMap(), 10))

	tests := []struct {
		name                string
		trs                 []*v1beta1.TaskRun
		prs                 []*v1beta1.PipelineRun
		expectedPipelineRun *v1beta1.PipelineRun
		expectedTaskRuns    []*v1beta1.TaskRun
	}{{
		name: "matrixed pipelinetask with retries, where one taskrun has failed and another one is running",
		trs: []*v1beta1.TaskRun{
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "False"
`),
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "Unknown"
`),
		},
		prs: []*v1beta1.PipelineRun{
			parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p
status:
  pipelineSpec:
    tasks:
    - name: platforms-and-browsers
      retries: 1
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
      params:
        - name: browser
          value: chrome
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
		},
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p
status:
  pipelineSpec:
    tasks:
    - name: platforms-and-browsers
      retries: 1
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
      params:
        - name: browser
          value: chrome
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
		expectedTaskRuns: []*v1beta1.TaskRun{
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "Unknown"
  retriesStatus:
  - conditions:
    - status: "False"
      type: Succeeded
`),
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "Unknown"
`),
		},
	}, {
		name: "matrixed pipelinetask with retries, where both taskruns have failed",
		trs: []*v1beta1.TaskRun{
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "False"
`),
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "False"
`),
		},
		prs: []*v1beta1.PipelineRun{
			parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p
status:
  pipelineSpec:
    tasks:
    - name: platforms-and-browsers
      retries: 1
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
      params:
        - name: browser
          value: chrome
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
		},
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p
status:
  pipelineSpec:
    tasks:
    - name: platforms-and-browsers
      retries: 1
      taskRef:
        name: mytask
        kind: Task
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
      params:
        - name: browser
          value: chrome
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
		expectedTaskRuns: []*v1beta1.TaskRun{
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "Unknown"
  retriesStatus:
  - conditions:
    - status: "False"
      type: Succeeded
`),
			mustParseTaskRunWithObjectMeta(t,
				taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
					"pr", "p", "platforms-and-browsers", false),
				`
spec:
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
  conditions:
  - type: Succeeded
    status: "Unknown"
  retriesStatus:
  - conditions:
    - status: "False"
      type: Succeeded
`),
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := test.Data{
				Tasks:        []*v1beta1.Task{task},
				TaskRuns:     tt.trs,
				PipelineRuns: tt.prs,
				ConfigMaps:   cms,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			_, clients := prt.reconcileRun("foo", "pr", []string{}, false)
			taskRuns, err := clients.Pipeline.TektonV1beta1().TaskRuns("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("tekton.dev/pipelineRun=pr,tekton.dev/pipelineTask=platforms-and-browsers"),
				Limit:         1,
			})
			if err != nil {
				t.Fatalf("Failure to list TaskRun's %s", err)
			}

			if len(taskRuns.Items) != 2 {
				t.Fatalf("Expected 2 TaskRuns got %d", len(taskRuns.Items))
			}

			for i := range taskRuns.Items {
				expectedTaskRun := tt.expectedTaskRuns[i]
				if d := cmp.Diff(expectedTaskRun, &taskRuns.Items[i], ignoreResourceVersion, ignoreTypeMeta, ignoreLastTransitionTime, ignoreStartTime); d != "" {
					t.Errorf("expected to see TaskRun %v created. Diff %s", tt.expectedTaskRuns[i].Name, diff.PrintWantGot(d))
				}
			}

			pipelineRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(prt.TestAssets.Ctx, "pr", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Got an error getting reconciled run out of fake client: %s", err)
			}
			if d := cmp.Diff(tt.expectedPipelineRun, pipelineRun, ignoreResourceVersion, ignoreTypeMeta, ignoreLastTransitionTime, ignoreStartTime, cmpopts.SortSlices(lessChildReferences)); d != "" {
				t.Errorf("expected PipelineRun was not created. Diff %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestReconciler_PipelineTaskMatrixWithCustomTask(t *testing.T) {
	names.TestingSeed()

	task := parse.MustParseTask(t, `
metadata:
  name: mytask
  namespace: foo
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
    - name: platform
    - name: browser
    - name: version
  steps:
    - name: echo
      image: alpine
      script: |
        echo "$(params.platform) and $(params.browser)" and $(params.version)"
`)

	expectedRuns := []*v1alpha1.Run{
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-0", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-1", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: mac
  - name: browser
    value: chrome
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-2", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: windows
  - name: browser
    value: chrome
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-3", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: linux
  - name: browser
    value: safari
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-4", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: mac
  - name: browser
    value: safari
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-5", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: windows
  - name: browser
    value: safari
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-6", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: linux
  - name: browser
    value: firefox
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-7", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: mac
  - name: browser
    value: firefox
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
		mustParseRunWithObjectMeta(t,
			taskRunObjectMeta("pr-platforms-and-browsers-8", "foo",
				"pr", "p", "platforms-and-browsers", false),
			`
spec:
  ref:
    apiVersion: example.dev/v0
    kind: Example
  params:
  - name: platform
    value: windows
  - name: browser
    value: firefox
  - name: version
    value: v0.1
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
`),
	}
	cms := []*corev1.ConfigMap{withEmbeddedStatus(withEnabledAlphaAPIFields(newFeatureFlagsConfigMap()), config.MinimalEmbeddedStatus)}

	tests := []struct {
		name                string
		memberOf            string
		p                   *v1beta1.Pipeline
		tr                  *v1beta1.TaskRun
		expectedPipelineRun *v1beta1.PipelineRun
	}{{
		name:     "p-dag",
		memberOf: "tasks",
		p: parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  tasks:
    - name: platforms-and-browsers
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.1
`, "p-dag")),
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p-dag
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p-dag
status:
  pipelineSpec:
    tasks:
    - name: platforms-and-browsers
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.1
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 0 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-2
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-3
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-4
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-5
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-6
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-7
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-8
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
	}, {
		name:     "p-finally",
		memberOf: "finally",
		p: parse.MustParsePipeline(t, fmt.Sprintf(`
metadata:
  name: %s
  namespace: foo
spec:
  tasks:
    - name: unmatrixed-pt
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.0
      taskRef:
        name: mytask
  finally:
    - name: platforms-and-browsers
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.1
`, "p-finally")),
		tr: mustParseTaskRunWithObjectMeta(t,
			taskRunObjectMeta("pr-unmatrixed-pt", "foo",
				"pr", "p-finally", "unmatrixed-pt", false),
			`
spec:
  params:
  - name: platform
    value: linux
  - name: browser
    value: chrome
  - name: version
    value: v0.0
  resources: {}
  serviceAccountName: test-sa
  taskRef:
    name: mytask
  timeout: 1h0m0s
status:
 conditions:
  - type: Succeeded
    status: "True"
    reason: Succeeded
    message: All Tasks have completed executing
`),
		expectedPipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: pr
  namespace: foo
  annotations: {}
  labels:
    tekton.dev/pipeline: p-finally
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: p-finally
status:
  pipelineSpec:
    tasks:
    - name: unmatrixed-pt
      params:
        - name: platform
          value: linux
        - name: browser
          value: chrome
        - name: version
          value: v0.0
      taskRef:
        name: mytask
        kind: Task
    finally:
    - name: platforms-and-browsers
      taskRef:
        apiVersion: example.dev/v0
        kind: Example
      matrix:
        params:
          - name: platform
            value:
              - linux
              - mac
              - windows
          - name: browser
            value:
              - chrome
              - safari
              - firefox
      params:
        - name: version
          value: v0.1
  conditions:
  - type: Succeeded
    status: "Unknown"
    reason: "Running"
    message: "Tasks Completed: 1 (Failed: 0, Cancelled 0), Incomplete: 1, Skipped: 0"
  childReferences:
  - apiVersion: tekton.dev/v1beta1
    kind: TaskRun
    name: pr-unmatrixed-pt
    pipelineTaskName: unmatrixed-pt
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-0
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-1
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-2
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-3
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-4
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-5
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-6
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-7
    pipelineTaskName: platforms-and-browsers
  - apiVersion: tekton.dev/v1alpha1
    kind: Run
    name: pr-platforms-and-browsers-8
    pipelineTaskName: platforms-and-browsers
  taskRuns: {}
  runs: {}
`),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := parse.MustParsePipelineRun(t, fmt.Sprintf(`
metadata:
  name: pr
  namespace: foo
spec:
  serviceAccountName: test-sa
  pipelineRef:
    name: %s
`, tt.name))
			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{pr},
				Pipelines:    []*v1beta1.Pipeline{tt.p},
				Tasks:        []*v1beta1.Task{task},
				ConfigMaps:   cms,
			}
			if tt.tr != nil {
				d.TaskRuns = []*v1beta1.TaskRun{tt.tr}
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			_, clients := prt.reconcileRun("foo", "pr", []string{}, false)
			runs, err := clients.Pipeline.TektonV1alpha1().Runs("foo").List(prt.TestAssets.Ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("tekton.dev/pipelineRun=pr,tekton.dev/pipeline=%s,tekton.dev/pipelineTask=platforms-and-browsers", tt.name),
				Limit:         1,
			})
			if err != nil {
				t.Fatalf("Failure to list TaskRun's %s", err)
			}

			if len(runs.Items) != 9 {
				t.Fatalf("Expected 9 TaskRuns got %d", len(runs.Items))
			}

			for i := range runs.Items {
				expectedTaskRun := expectedRuns[i]
				expectedTaskRun.Labels["tekton.dev/pipeline"] = tt.name
				expectedTaskRun.Labels["tekton.dev/memberOf"] = tt.memberOf
				if d := cmp.Diff(expectedTaskRun, &runs.Items[i], ignoreResourceVersion, ignoreTypeMeta); d != "" {
					t.Errorf("expected to see TaskRun %v created. Diff %s", expectedRuns[i].Name, diff.PrintWantGot(d))
				}
			}

			pipelineRun, err := clients.Pipeline.TektonV1beta1().PipelineRuns("foo").Get(prt.TestAssets.Ctx, "pr", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Got an error getting reconciled run out of fake client: %s", err)
			}
			if d := cmp.Diff(tt.expectedPipelineRun, pipelineRun, ignoreResourceVersion, ignoreTypeMeta, ignoreLastTransitionTime, ignoreStartTime, ignoreFinallyStartTime); d != "" {
				t.Errorf("expected PipelineRun was not created. Diff %s", diff.PrintWantGot(d))
			}
		})
	}
}

func lessTaskResourceBindings(i, j v1beta1.TaskResourceBinding) bool {
	return i.Name < j.Name
}

func TestReconcile_SetDefaults(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			runTestReconcileWithoutDefaults(t, tc.embeddedStatusVal)
		})
	}
}

func runTestReconcileWithoutDefaults(t *testing.T, embeddedStatus string) {
	names.TestingSeed()
	prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `
metadata:
  name: test-pipeline-run-success
  namespace: foo
spec:
  params:
  - name: bar
    value: somethingmorefun
  pipelineRef:
    name: test-pipeline
  serviceAccountName: test-sa
`)}
	ps := []*v1beta1.Pipeline{parse.MustParsePipeline(t, `
metadata:
  name: test-pipeline
  namespace: foo
spec:
  params:
  - default: somethingdifferent
    name: pipeline-param
  - default: revision
    name: rev-param
  - name: bar
  tasks:
  - name: unit-test-1
    params:
    - name: foo
      value: somethingfun
    - name: bar
      value: $(params.bar)
    taskRef:
      name: unit-test-task
  - name: unit-test-cluster-task
    params:
    - name: foo
      value: somethingfun
    - name: bar
      value: $(params.bar)
    taskRef:
      kind: ClusterTask
      name: unit-test-cluster-task
`)}
	ts := []*v1beta1.Task{
		parse.MustParseTask(t, `
metadata:
  name: unit-test-task
  namespace: foo
spec:
  params:
  - name: foo
  - name: bar
`),
	}
	clusterTasks := []*v1beta1.ClusterTask{
		parse.MustParseClusterTask(t, `
metadata:
  name: unit-test-cluster-task
spec:
  params:
  - name: foo
  - name: bar
`),
	}

	d := test.Data{
		PipelineRuns: prs,
		Pipelines:    ps,
		Tasks:        ts,
		ClusterTasks: clusterTasks,
		ConfigMaps:   []*corev1.ConfigMap{withEmbeddedStatus(newFeatureFlagsConfigMap(), embeddedStatus)},
	}
	prt := newPipelineRunTest(d, t)
	defer prt.Cancel()

	wantEvents := []string{
		"Normal Started",
		"Normal Running Tasks Completed: 0",
	}
	reconciledRun, clients := prt.reconcileRun("foo", "test-pipeline-run-success", wantEvents, false)

	// Check that the expected TaskRun was created
	actual := getTaskRunCreations(t, clients.Pipeline.Actions(), 2)[0]
	expectedTaskRun := mustParseTaskRunWithObjectMeta(t,
		taskRunObjectMeta("test-pipeline-run-success-unit-test-1", "foo", "test-pipeline-run-success",
			"test-pipeline", "unit-test-1", false),
		`
spec:
  params:
  - name: foo
    value: somethingfun
  - name: bar
    value: somethingmorefun
  serviceAccountName: test-sa
  taskRef:
    name: unit-test-task
    kind: Task
  resources: {}
`)
	// ignore IgnoreUnexported ignore both after and before steps fields
	if d := cmp.Diff(expectedTaskRun, actual, ignoreTypeMeta, cmpopts.SortSlices(lessTaskResourceBindings)); d != "" {
		t.Errorf("expected to see TaskRun %v created. Diff %s", expectedTaskRun, diff.PrintWantGot(d))
	}

	// This PipelineRun is in progress now and the status should reflect that
	checkPipelineRunConditionStatusAndReason(t, reconciledRun, corev1.ConditionUnknown, v1beta1.PipelineRunReasonRunning.String())

	tr1Name := "test-pipeline-run-success-unit-test-1"
	tr2Name := "test-pipeline-run-success-unit-test-cluster-task"

	verifyTaskRunStatusesCount(t, embeddedStatus, reconciledRun.Status, 2)
	verifyTaskRunStatusesNames(t, embeddedStatus, reconciledRun.Status, tr1Name, tr2Name)
}

func TestReconcile_CreateTaskRunWithComputeResources(t *testing.T) {
	simplePipeline := parse.MustParsePipeline(t, `
metadata:
  name: foo-pipeline
  namespace: default
spec:
  tasks:
  - name: 1st-task
    taskSpec:
      steps:
      - name: 1st-step
        image: foo-image
        command:
        - /foo-cmd
      - name: 2nd-step
        image: foo-image
        command:
        - /foo-cmd
`)

	testCases := []struct {
		name                     string
		pipeline                 *v1beta1.Pipeline
		pipelineRun              *v1beta1.PipelineRun
		expectedComputeResources []corev1.ResourceRequirements
	}{{
		name:     "only with requests",
		pipeline: simplePipeline,
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: foo-pipeline-run
  namespace: default
spec:
  pipelineRef:
    name: foo-pipeline
  taskRunSpecs:
  - pipelineTaskName: 1st-task
    computeResources:
      requests:
        cpu: "500m"
`),
		expectedComputeResources: []corev1.ResourceRequirements{{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
		}},
	}, {
		name:     "only with limits",
		pipeline: simplePipeline,
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: foo-pipeline-run
  namespace: default
spec:
  pipelineRef:
    name: foo-pipeline
  taskRunSpecs:
  - pipelineTaskName: 1st-task
    computeResources:
      limits:
        cpu: "2"
`),
		expectedComputeResources: []corev1.ResourceRequirements{{
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}},
	}, {
		name:     "both with requests and limits",
		pipeline: simplePipeline,
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: foo-pipeline-run
  namespace: default
spec:
  pipelineRef:
    name: foo-pipeline
  taskRunSpecs:
  - pipelineTaskName: 1st-task
    computeResources:
      requests:
        cpu: "500m"
      limits:
        cpu: "2"
`),
		expectedComputeResources: []corev1.ResourceRequirements{{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}},
	}, {
		name:     "both with cpu and memory",
		pipeline: simplePipeline,
		pipelineRun: parse.MustParsePipelineRun(t, `
metadata:
  name: foo-pipeline-run
  namespace: default
spec:
  pipelineRef:
    name: foo-pipeline
  taskRunSpecs:
  - pipelineTaskName: 1st-task
    computeResources:
      requests:
        cpu: "1"
        memory: "1Gi"
      limits:
        cpu: "2"
`),
		expectedComputeResources: []corev1.ResourceRequirements{{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi")},
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		}},
	}}

	// verifyTaskLevelComputeResources verifies that the created TaskRuns have the expected compute resources
	verifyTaskLevelComputeResources := func(expectedComputeResources []corev1.ResourceRequirements, taskRuns []v1beta1.TaskRun) error {
		if len(expectedComputeResources) != len(taskRuns) {
			return fmt.Errorf("expected %d compute resource requirements, got %d", len(expectedComputeResources), len(taskRuns))
		}
		for i, r := range expectedComputeResources {
			if d := cmp.Diff(r, *taskRuns[i].Spec.ComputeResources); d != "" {
				return fmt.Errorf("TaskRun #%d resource requirements don't match %s", i, diff.PrintWantGot(d))
			}
		}
		return nil
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := test.Data{
				PipelineRuns: []*v1beta1.PipelineRun{tc.pipelineRun},
				Pipelines:    []*v1beta1.Pipeline{tc.pipeline},
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			reconciledRun, clients := prt.reconcileRun("default", tc.pipelineRun.Name, []string{}, false)

			if reconciledRun.Status.CompletionTime != nil {
				t.Errorf("Expected a CompletionTime on valid PipelineRun, but got nil")
			}

			TaskRunList, err := clients.Pipeline.TektonV1beta1().TaskRuns("default").List(prt.TestAssets.Ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("Failure to list TaskRun's %s", err)
			}

			if err := verifyTaskLevelComputeResources(tc.expectedComputeResources, TaskRunList.Items); err != nil {
				t.Errorf("TaskRun \"%s\" failed to verify task-level compute resource requirements, because: %v", tc.name, err)
			}
		})
	}
}

func TestReconcile_CancelUnscheduled(t *testing.T) {
	testCases := []struct {
		name              string
		embeddedStatusVal string
	}{
		{
			name:              "default embedded status",
			embeddedStatusVal: config.DefaultEmbeddedStatus,
		},
		{
			name:              "full embedded status",
			embeddedStatusVal: config.FullEmbeddedStatus,
		},
		{
			name:              "both embedded status",
			embeddedStatusVal: config.BothEmbeddedStatus,
		},
		{
			name:              "minimal embedded status",
			embeddedStatusVal: config.MinimalEmbeddedStatus,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pipelineRunName := "cancel-test-run"
			prs := []*v1beta1.PipelineRun{parse.MustParsePipelineRun(t, `metadata:
  name: cancel-test-run
  namespace: foo
spec:
  pipelineSpec:
    tasks:
      - name: wait-1
        taskSpec:
          apiVersion: example.dev/v0
          kind: Wait
          params:
            - name: duration
              value: 1h
      - name: wait-2
        runAfter:
          - wait-1
        taskSpec:
          apiVersion: example.dev/v0
          kind: Wait
          params:
            - name: duration
              value: 10s
      - name: wait-3
        runAfter:
          - wait-1
        taskRef:
          name: hello-world
`)}

			ts := []*v1beta1.Task{simpleHelloWorldTask}

			cms := []*corev1.ConfigMap{withEmbeddedStatus(withCustomTasks(newFeatureFlagsConfigMap()), tc.embeddedStatusVal)}

			d := test.Data{
				PipelineRuns: prs,
				Tasks:        ts,
				ConfigMaps:   cms,
			}
			prt := newPipelineRunTest(d, t)
			defer prt.Cancel()

			pr, clients := prt.reconcileRun("foo", pipelineRunName, []string{}, false)

			if tc.embeddedStatusVal != config.MinimalEmbeddedStatus {
				if len(pr.Status.Runs) > 1 {
					t.Errorf("Expected one Run in status, but found %d", len(pr.Status.Runs))
				}
				if len(pr.Status.TaskRuns) > 0 {
					t.Errorf("Expected no TaskRuns in status, but found %d", len(pr.Status.TaskRuns))
				}
			}
			if tc.embeddedStatusVal != config.FullEmbeddedStatus {
				if len(pr.Status.ChildReferences) > 1 {
					t.Errorf("Expected one Run or TaskRun in child references, but found %d", len(pr.Status.ChildReferences))
				}
			}

			err := cancelPipelineRun(prt.TestAssets.Ctx, logtesting.TestLogger(t), pr, clients.Pipeline)
			if err != nil {
				t.Fatalf("Error found: %v", err)
			}
		})
	}
}
