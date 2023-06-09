/*
Copyright 2017 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sparkapplication

import (
	"errors"
	"fmt"

	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	prometheus_model "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	kubeclientfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/apis/sparkoperator.k8s.io/v1beta2"
	crdclientfake "github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/client/clientset/versioned/fake"
	crdinformers "github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/client/informers/externalversions"
	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/config"
	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/util"
)

func newFakeController(app *v1beta2.SparkApplication, jobManager submissionJobManager, pods ...*apiv1.Pod) (*Controller, *record.FakeRecorder) {
	crdclientfake.AddToScheme(scheme.Scheme)
	crdClient := crdclientfake.NewSimpleClientset()
	kubeClient := kubeclientfake.NewSimpleClientset()
	informerFactory := crdinformers.NewSharedInformerFactory(crdClient, 0*time.Second)
	recorder := record.NewFakeRecorder(3)

	kubeClient.CoreV1().Nodes().Create(&apiv1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
		},
		Status: apiv1.NodeStatus{
			Addresses: []apiv1.NodeAddress{
				{
					Type:    apiv1.NodeExternalIP,
					Address: "12.34.56.78",
				},
			},
		},
	})

	podInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0*time.Second)
	controller := newSparkApplicationController(crdClient, kubeClient, informerFactory, podInformerFactory, recorder,
		&util.MetricConfig{}, "", nil, true)
	controller.subJobManager = jobManager
	informer := informerFactory.Sparkoperator().V1beta2().SparkApplications().Informer()
	if app != nil {
		informer.GetIndexer().Add(app)
	}

	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	for _, pod := range pods {
		if pod != nil {
			podInformer.GetIndexer().Add(pod)
		}
	}
	return controller, recorder
}

func TestOnAdd(t *testing.T) {
	ctrl, _ := newFakeController(nil, nil)

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Status: v1beta2.SparkApplicationStatus{},
	}
	ctrl.onAdd(app)

	item, _ := ctrl.queue.Get()
	defer ctrl.queue.Done(item)
	key, ok := item.(string)
	assert.True(t, ok)
	expectedKey, _ := cache.MetaNamespaceKeyFunc(app)
	assert.Equal(t, expectedKey, key)
	ctrl.queue.Forget(item)
}

func TestOnUpdate(t *testing.T) {
	ctrl, recorder := newFakeController(nil, nil)

	appTemplate := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "foo",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: v1beta2.SparkApplicationSpec{
			Mode:  v1beta2.ClusterMode,
			Image: stringptr("foo-image:v1"),
			Executor: v1beta2.ExecutorSpec{
				Instances: int32ptr(1),
			},
		},
	}

	// Case1: Same Spec.
	copyWithSameSpec := appTemplate.DeepCopy()
	copyWithSameSpec.Status.ExecutionAttempts = 3
	copyWithSameSpec.ResourceVersion = "2"

	ctrl.onUpdate(appTemplate, copyWithSameSpec)

	// Verify that the SparkApplication was enqueued but no spec update events fired.
	item, _ := ctrl.queue.Get()
	key, ok := item.(string)
	assert.True(t, ok)
	expectedKey, _ := cache.MetaNamespaceKeyFunc(appTemplate)
	assert.Equal(t, expectedKey, key)
	ctrl.queue.Forget(item)
	ctrl.queue.Done(item)
	assert.Equal(t, 0, len(recorder.Events))

	// Case2: Spec update failed.
	copyWithSpecUpdate := appTemplate.DeepCopy()
	copyWithSpecUpdate.Spec.Image = stringptr("foo-image:v2")
	copyWithSpecUpdate.ResourceVersion = "2"

	ctrl.onUpdate(appTemplate, copyWithSpecUpdate)

	// Verify that ppdate failed due to non-existance of SparkApplication.
	assert.Equal(t, 1, len(recorder.Events))
	event := <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationSpecUpdateFailed"))

	// Case3: Spec update successful.
	ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(appTemplate.Namespace).Create(appTemplate)
	ctrl.onUpdate(appTemplate, copyWithSpecUpdate)

	// Verify App was enqueued.
	item, _ = ctrl.queue.Get()
	key, ok = item.(string)
	assert.True(t, ok)
	expectedKey, _ = cache.MetaNamespaceKeyFunc(appTemplate)
	assert.Equal(t, expectedKey, key)
	ctrl.queue.Forget(item)
	ctrl.queue.Done(item)
	// Verify that update was succeeded.
	assert.Equal(t, 1, len(recorder.Events))
	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationSpecUpdateProcessed"))

	// Verify the SparkApplication state was updated to InvalidatingState.
	app, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(appTemplate.Namespace).Get(appTemplate.Name, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, v1beta2.InvalidatingState, app.Status.AppState.State)
}

func TestOnDelete(t *testing.T) {
	mockJobManager := fakeSubmissionJobManager{
		deleteSubmissionJobCb: func(app *v1beta2.SparkApplication) error {
			return nil
		},
	}
	ctrl, recorder := newFakeController(nil, &mockJobManager)

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Status: v1beta2.SparkApplicationStatus{},
	}
	ctrl.onAdd(app)
	ctrl.queue.Get()

	ctrl.onDelete(app)
	ctrl.queue.ShutDown()
	item, _ := ctrl.queue.Get()
	defer ctrl.queue.Done(item)
	assert.True(t, item == nil)
	event := <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationDeleted"))
	ctrl.queue.Forget(item)
}

func TestHelperProcessFailure(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(2)
}

func TestHelperProcessSuccess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(0)
}

func fetchCounterValue(m *prometheus.CounterVec, labels map[string]string) float64 {
	pb := &prometheus_model.Metric{}
	m.With(labels).Write(pb)

	return pb.GetCounter().GetValue()
}

type metrics struct {
	submitMetricCount  float64
	runningMetricCount float64
	successMetricCount float64
	failedMetricCount  float64
}

type executorMetrics struct {
	runningMetricCount float64
	successMetricCount float64
	failedMetricCount  float64
}

type fakeSubmissionJobManager struct {
	createSubmissionJobCb func(app *v1beta2.SparkApplication) (string, string, error)
	deleteSubmissionJobCb func(app *v1beta2.SparkApplication) error
	getSubmissionJobCb    func(app *v1beta2.SparkApplication) (*batchv1.Job, error)
	hasJobSucceededCb     func(app *v1beta2.SparkApplication) (*bool, *metav1.Time, error)
}

func (m *fakeSubmissionJobManager) createSubmissionJob(app *v1beta2.SparkApplication) (string, string, error) {
	return m.createSubmissionJobCb(app)
}

func (m *fakeSubmissionJobManager) deleteSubmissionJob(app *v1beta2.SparkApplication) error {
	return m.deleteSubmissionJobCb(app)
}

func (m *fakeSubmissionJobManager) getSubmissionJob(app *v1beta2.SparkApplication) (*batchv1.Job, error) {
	return m.getSubmissionJobCb(app)
}

func (m *fakeSubmissionJobManager) hasJobSucceeded(app *v1beta2.SparkApplication) (*bool, *metav1.Time, error) {
	return m.hasJobSucceededCb(app)
}

func TestSyncSparkApplication_Submission(t *testing.T) {
	os.Setenv(kubernetesServiceHostEnvVar, "localhost")
	os.Setenv(kubernetesServicePortEnvVar, "443")

	restartPolicyOnFailure := v1beta2.RestartPolicy{
		Type:                       v1beta2.OnFailure,
		OnFailureRetries:           int32ptr(1),
		OnFailureRetryInterval:     int64ptr(100),
		OnSubmissionFailureRetries: int32ptr(1),
	}
	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Spec: v1beta2.SparkApplicationSpec{
			Mode:          v1beta2.ClusterMode,
			RestartPolicy: restartPolicyOnFailure,
		},
		Status: v1beta2.SparkApplicationStatus{
			AppState: v1beta2.ApplicationState{
				State:        v1beta2.NewState,
				ErrorMessage: "",
			},
		},
	}

	// Case 1: Submission Failed.
	mockJobManager := fakeSubmissionJobManager{
		createSubmissionJobCb: func(app *v1beta2.SparkApplication) (string, string, error) {
			return "", "", errors.New("Failed to submit app")
		},
	}
	ctrl, recorder := newFakeController(app, &mockJobManager)
	_, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Create(app)
	if err != nil {
		t.Fatal(err)
	}
	err = ctrl.syncSparkApplication("default/foo")
	updatedApp, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Get(app.Name, metav1.GetOptions{})
	assert.Equal(t, v1beta2.FailedSubmissionState, updatedApp.Status.AppState.State)
	assert.Equal(t, int32(1), updatedApp.Status.SubmissionAttempts)
	assert.Equal(t, float64(1), fetchCounterValue(ctrl.metrics.sparkAppCount, map[string]string{}))
	assert.Equal(t, float64(0), fetchCounterValue(ctrl.metrics.sparkAppSubmitCount, map[string]string{}))
	assert.Equal(t, float64(1), fetchCounterValue(ctrl.metrics.sparkAppFailedSubmissionCount, map[string]string{}))

	// Validate Events
	event := <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationAdded"))
	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationSubmissionFailed"))

	// Case 2: Job Creation Succeeded.
	mockJobManager = fakeSubmissionJobManager{
		createSubmissionJobCb: func(app *v1beta2.SparkApplication) (string, string, error) {
			return "uuid", "foo-driver", nil
		},
		hasJobSucceededCb: func(app *v1beta2.SparkApplication) (*bool, *metav1.Time, error) {
			return boolptr(true), nil, nil
		},
	}
	ctrl, recorder = newFakeController(app, &mockJobManager)
	_, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Create(app)
	if err != nil {
		t.Fatal(err)
	}

	err = ctrl.syncSparkApplication("default/foo")
	updatedApp, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Get(app.Name, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, v1beta2.PendingSubmissionState, updatedApp.Status.AppState.State)
	assert.Equal(t, float64(0), fetchCounterValue(ctrl.metrics.sparkAppSubmitCount, map[string]string{}))

	// Validate Events
	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationAdded"))
	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SubmissionJobCreated"))

	//Case 3: Submission Success
	mockJobManager = fakeSubmissionJobManager{
		hasJobSucceededCb: func(app *v1beta2.SparkApplication) (*bool, *metav1.Time, error) {
			return boolptr(true), nil, nil
		},
	}
	ctrl, recorder = newFakeController(updatedApp, &mockJobManager)
	_, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Create(updatedApp)
	if err != nil {
		t.Fatal(err)
	}
	err = ctrl.syncSparkApplication("default/foo")

	updatedApp, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Get(app.Name, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, v1beta2.SubmittedState, updatedApp.Status.AppState.State)
	assert.Equal(t, float64(1), fetchCounterValue(ctrl.metrics.sparkAppSubmitCount, map[string]string{}))

	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationSubmitted"))

	// Case 4: Pending Rerun -> Submission Job Created
	mockJobManager = fakeSubmissionJobManager{
		createSubmissionJobCb: func(app *v1beta2.SparkApplication) (string, string, error) {
			return "uuid", "foo-driver", nil
		},
		getSubmissionJobCb: func(app *v1beta2.SparkApplication) (job *batchv1.Job, e error) {
			return nil, &apiErrors.StatusError{
				ErrStatus: metav1.Status{
					Code:   http.StatusNotFound,
					Reason: metav1.StatusReasonNotFound,
				}}
		},
	}
	app.Status.AppState.State = v1beta2.PendingRerunState
	ctrl, recorder = newFakeController(app, &mockJobManager)
	_, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Create(app)
	if err != nil {
		t.Fatal(err)
	}

	err = ctrl.syncSparkApplication("default/foo")
	updatedApp, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Get(app.Name, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, v1beta2.PendingSubmissionState, updatedApp.Status.AppState.State)

	// Validate Events
	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SparkApplicationPendingRerun"))
	event = <-recorder.Events
	assert.True(t, strings.Contains(event, "SubmissionJobCreated"))
}

func TestValidateDetectsNodeSelectorSuccessNoSelector(t *testing.T) {
	ctrl, _ := newFakeController(nil, nil)

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
	}

	err := ctrl.validateSparkApplication(app)
	assert.Nil(t, err)
}

func TestValidateDetectsNodeSelectorSuccessNodeSelectorAtAppLevel(t *testing.T) {
	ctrl, _ := newFakeController(nil, nil)

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Spec: v1beta2.SparkApplicationSpec{
			NodeSelector: map[string]string{"mynode": "mygift"},
		},
	}

	err := ctrl.validateSparkApplication(app)
	assert.Nil(t, err)
}

func TestValidateDetectsNodeSelectorSuccessNodeSelectorAtPodLevel(t *testing.T) {
	ctrl, _ := newFakeController(nil, nil)

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Spec: v1beta2.SparkApplicationSpec{
			Driver: v1beta2.DriverSpec{
				SparkPodSpec: v1beta2.SparkPodSpec{
					NodeSelector: map[string]string{"mynode": "mygift"},
				},
			},
		},
	}

	err := ctrl.validateSparkApplication(app)
	assert.Nil(t, err)

	app.Spec.Executor = v1beta2.ExecutorSpec{
		SparkPodSpec: v1beta2.SparkPodSpec{
			NodeSelector: map[string]string{"mynode": "mygift"},
		},
	}

	err = ctrl.validateSparkApplication(app)
	assert.Nil(t, err)
}

func TestValidateDetectsNodeSelectorFailsAppAndPodLevel(t *testing.T) {
	ctrl, _ := newFakeController(nil, nil)

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
		Spec: v1beta2.SparkApplicationSpec{
			NodeSelector: map[string]string{"mynode": "mygift"},
			Driver: v1beta2.DriverSpec{
				SparkPodSpec: v1beta2.SparkPodSpec{
					NodeSelector: map[string]string{"mynode": "mygift"},
				},
			},
		},
	}

	err := ctrl.validateSparkApplication(app)
	assert.NotNil(t, err)

	app.Spec.Executor = v1beta2.ExecutorSpec{
		SparkPodSpec: v1beta2.SparkPodSpec{
			NodeSelector: map[string]string{"mynode": "mygift"},
		},
	}

	err = ctrl.validateSparkApplication(app)
	assert.NotNil(t, err)
}

//change this test back to true, as of now retries are based on onFailureRetries
func TestShouldRetry(t *testing.T) {
	type testcase struct {
		app         *v1beta2.SparkApplication
		shouldRetry bool
	}

	testFn := func(test testcase, t *testing.T) {
		shouldRetry := shouldRetry(test.app)
		assert.Equal(t, test.shouldRetry, shouldRetry)
	}

	restartPolicyAlways := v1beta2.RestartPolicy{
		Type:                   v1beta2.Always,
		OnFailureRetryInterval: int64ptr(100),
	}

	restartPolicyNever := v1beta2.RestartPolicy{
		Type: v1beta2.Never,
	}

	restartPolicyOnFailure := v1beta2.RestartPolicy{
		Type:                       v1beta2.OnFailure,
		OnFailureRetries:           int32ptr(1),
		OnFailureRetryInterval:     int64ptr(100),
		OnSubmissionFailureRetries: int32ptr(2),
	}

	testcases := []testcase{
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				}},
			shouldRetry: false,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyAlways,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.SucceedingState,
					},
				},
			},
			shouldRetry: true,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					RestartPolicy: restartPolicyOnFailure,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.SucceedingState,
					},
				},
			},
			shouldRetry: false,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					RestartPolicy: restartPolicyOnFailure,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
				},
			},
			shouldRetry: true,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					RestartPolicy: restartPolicyNever,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
				},
			},
			shouldRetry: false,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					RestartPolicy: restartPolicyNever,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailedSubmissionState,
					},
				},
			},
			shouldRetry: false,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					RestartPolicy: restartPolicyOnFailure,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailedSubmissionState,
					},
				},
			},
			shouldRetry: true,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					RestartPolicy: restartPolicyAlways,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.PendingRerunState,
					},
				},
			},
			shouldRetry: false,
		},
	}

	for _, test := range testcases {
		testFn(test, t)
	}
}

func TestSyncSparkApplication_SubmissionSuccess(t *testing.T) {
	type testcase struct {
		app           *v1beta2.SparkApplication
		expectedState v1beta2.ApplicationStateType
	}
	os.Setenv(kubernetesServiceHostEnvVar, "localhost")
	os.Setenv(kubernetesServicePortEnvVar, "443")

	mockJobManager := fakeSubmissionJobManager{
		createSubmissionJobCb: func(app *v1beta2.SparkApplication) (string, string, error) {
			return "uuid", "foo-driver", nil
		},
		hasJobSucceededCb: func(app *v1beta2.SparkApplication) (*bool, *metav1.Time, error) {
			return boolptr(true), nil, nil
		},
		deleteSubmissionJobCb: func(app *v1beta2.SparkApplication) error {
			return nil
		},
		getSubmissionJobCb: func(app *v1beta2.SparkApplication) (job *batchv1.Job, e error) {
			return &batchv1.Job{}, nil
		},
	}

	testFn := func(test testcase, t *testing.T) {
		ctrl, _ := newFakeController(test.app, &mockJobManager)
		_, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(test.app.Namespace).Create(test.app)
		if err != nil {
			t.Fatal(err)
		}

		err = ctrl.syncSparkApplication(fmt.Sprintf("%s/%s", test.app.Namespace, test.app.Name))
		assert.Nil(t, err)
		updatedApp, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(test.app.Namespace).Get(test.app.Name, metav1.GetOptions{})
		assert.Nil(t, err)
		assert.Equal(t, test.expectedState, updatedApp.Status.AppState.State)
		if test.app.Status.AppState.State == v1beta2.NewState {
			assert.Equal(t, float64(1), fetchCounterValue(ctrl.metrics.sparkAppCount, map[string]string{}))
		}
		if test.expectedState == v1beta2.SubmittedState {
			assert.Equal(t, float64(1), fetchCounterValue(ctrl.metrics.sparkAppSubmitCount, map[string]string{}))
		}
	}

	restartPolicyAlways := v1beta2.RestartPolicy{
		Type:                   v1beta2.Always,
		OnFailureRetryInterval: int64ptr(100),
	}

	restartPolicyNever := v1beta2.RestartPolicy{
		Type: v1beta2.Never,
	}

	restartPolicyOnFailure := v1beta2.RestartPolicy{
		Type:                       v1beta2.OnFailure,
		OnFailureRetries:           int32ptr(1),
		OnFailureRetryInterval:     int64ptr(100),
		OnSubmissionFailureRetries: int32ptr(2),
	}

	testcases := []testcase{
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyAlways,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.SucceedingState,
					},
				},
			},
			expectedState: v1beta2.PendingRerunState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyAlways,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailedSubmissionState,
					},
				},
			},
			expectedState: v1beta2.PendingRerunState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyAlways,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
					ExecutionAttempts: 1,
					TerminationTime:   metav1.Time{Time: metav1.Now().Add(-2000 * time.Second)},
				},
			},
			expectedState: v1beta2.PendingRerunState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyAlways,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
					TerminationTime: metav1.Time{Time: metav1.Now().Add(-2000 * time.Second)},
				},
			},
			expectedState: v1beta2.FailingState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyNever,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.InvalidatingState,
					},
					TerminationTime: metav1.Time{Time: metav1.Now().Add(-2000 * time.Second)},
				},
			},
			expectedState: v1beta2.PendingRerunState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyNever,
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.SucceedingState,
					},
				},
			},
			expectedState: v1beta2.CompletedState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
					ExecutionAttempts: 2,
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyOnFailure,
				},
			},
			expectedState: v1beta2.FailedState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
					ExecutionAttempts: 1,
					TerminationTime:   metav1.Now(),
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyOnFailure,
				},
			},
			expectedState: v1beta2.FailingState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailingState,
					},
					ExecutionAttempts: 1,
					TerminationTime:   metav1.Time{Time: metav1.Now().Add(-2000 * time.Second)},
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyOnFailure,
				},
			},
			expectedState: v1beta2.PendingRerunState,
		},
		{
			app: &v1beta2.SparkApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "default",
				},
				Status: v1beta2.SparkApplicationStatus{
					AppState: v1beta2.ApplicationState{
						State: v1beta2.FailedSubmissionState,
					},
					SubmissionTime: metav1.Now(),
				},
				Spec: v1beta2.SparkApplicationSpec{
					Mode:          v1beta2.ClusterMode,
					RestartPolicy: restartPolicyOnFailure,
				},
			},
			expectedState: v1beta2.FailedState,
		},
	}

	for _, test := range testcases {
		testFn(test, t)
	}
}

func TestSyncSparkApplication_ExecutingState(t *testing.T) {
	type testcase struct {
		appName                 string
		oldAppStatus            v1beta2.ApplicationStateType
		oldExecutorStatus       map[string]v1beta2.ExecutorState
		driverPod               *apiv1.Pod
		executorPod             *apiv1.Pod
		expectedAppState        v1beta2.ApplicationStateType
		expectedExecutorState   map[string]v1beta2.ExecutorState
		expectedAppMetrics      metrics
		expectedExecutorMetrics executorMetrics
	}

	os.Setenv(kubernetesServiceHostEnvVar, "localhost")
	os.Setenv(kubernetesServicePortEnvVar, "443")

	appName := "foo"
	driverPodName := appName + "-driver"

	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: "test",
		},
		Spec: v1beta2.SparkApplicationSpec{
			Mode: v1beta2.ClusterMode,
			RestartPolicy: v1beta2.RestartPolicy{
				Type: v1beta2.Never,
			},
		},
		Status: v1beta2.SparkApplicationStatus{
			AppState: v1beta2.ApplicationState{
				State:        v1beta2.SubmittedState,
				ErrorMessage: "",
			},
			DriverInfo: v1beta2.DriverInfo{
				PodName: driverPodName,
			},
			ExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
		},
	}

	testcases := []testcase{
		{
			appName:               appName,
			oldAppStatus:          v1beta2.SubmittedState,
			oldExecutorStatus:     map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			expectedAppState:      v1beta2.FailingState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorFailedState},
			expectedAppMetrics: metrics{
				failedMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				failedMetricCount: 1,
			},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.SubmittedState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodRunning,
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			expectedAppState:      v1beta2.RunningState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics: metrics{
				runningMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				successMetricCount: 1,
			},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.RunningState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodRunning,
					ContainerStatuses: []apiv1.ContainerStatus{
						{
							Name: config.SparkDriverContainerName,
							State: apiv1.ContainerState{
								Running: &apiv1.ContainerStateRunning{},
							},
						},
						{
							Name: "sidecar",
							State: apiv1.ContainerState{
								Terminated: &apiv1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
					},
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			expectedAppState:      v1beta2.RunningState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics:    metrics{},
			expectedExecutorMetrics: executorMetrics{
				successMetricCount: 1,
			},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.RunningState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodRunning,
					ContainerStatuses: []apiv1.ContainerStatus{
						{
							Name: config.SparkDriverContainerName,
							State: apiv1.ContainerState{
								Terminated: &apiv1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
						{
							Name: "sidecar",
							State: apiv1.ContainerState{
								Running: &apiv1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			expectedAppState:      v1beta2.SucceedingState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics: metrics{
				successMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				successMetricCount: 1,
			},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.RunningState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodRunning,
					ContainerStatuses: []apiv1.ContainerStatus{
						{
							Name: config.SparkDriverContainerName,
							State: apiv1.ContainerState{
								Terminated: &apiv1.ContainerStateTerminated{
									ExitCode: 137,
									Reason:   "OOMKilled",
								},
							},
						},
						{
							Name: "sidecar",
							State: apiv1.ContainerState{
								Running: &apiv1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			expectedAppState:      v1beta2.FailingState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics: metrics{
				failedMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				successMetricCount: 1,
			},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.RunningState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodFailed,
					ContainerStatuses: []apiv1.ContainerStatus{
						{
							Name: config.SparkDriverContainerName,
							State: apiv1.ContainerState{
								Terminated: &apiv1.ContainerStateTerminated{
									ExitCode: 137,
									Reason:   "OOMKilled",
								},
							},
						},
					},
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodFailed,
				},
			},
			expectedAppState:      v1beta2.FailingState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorFailedState},
			expectedAppMetrics: metrics{
				failedMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				failedMetricCount: 1,
			},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.RunningState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodFailed,
					ContainerStatuses: []apiv1.ContainerStatus{
						{
							Name: config.SparkDriverContainerName,
							State: apiv1.ContainerState{
								Terminated: &apiv1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
						{
							Name: "sidecar",
							State: apiv1.ContainerState{
								Terminated: &apiv1.ContainerStateTerminated{
									ExitCode: 137,
									Reason:   "OOMKilled",
								},
							},
						},
					},
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			expectedAppState:      v1beta2.SucceedingState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics: metrics{
				successMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				successMetricCount: 1,
			},
		},
		{
			appName:                 appName,
			oldAppStatus:            v1beta2.FailingState,
			oldExecutorStatus:       map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorFailedState},
			expectedAppState:        v1beta2.FailedState,
			expectedExecutorState:   map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorFailedState},
			expectedAppMetrics:      metrics{},
			expectedExecutorMetrics: executorMetrics{},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.RunningState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorRunningState},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
					ResourceVersion: "1",
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodSucceeded,
				},
			},
			expectedAppState:      v1beta2.SucceedingState,
			expectedExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics: metrics{
				successMetricCount: 1,
			},
			expectedExecutorMetrics: executorMetrics{
				successMetricCount: 1,
			},
		},
		{
			appName:                 appName,
			oldAppStatus:            v1beta2.SucceedingState,
			oldExecutorStatus:       map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppState:        v1beta2.CompletedState,
			expectedExecutorState:   map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
			expectedAppMetrics:      metrics{},
			expectedExecutorMetrics: executorMetrics{},
		},
		{
			appName:           appName,
			oldAppStatus:      v1beta2.SubmittedState,
			oldExecutorStatus: map[string]v1beta2.ExecutorState{},
			driverPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      driverPodName,
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkDriverRole,
						config.SparkAppNameLabel: appName,
					},
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodUnknown,
				},
			},
			executorPod: &apiv1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "exec-1",
					Namespace: "test",
					Labels: map[string]string{
						config.SparkRoleLabel:    config.SparkExecutorRole,
						config.SparkAppNameLabel: appName,
					},
				},
				Status: apiv1.PodStatus{
					Phase: apiv1.PodPending,
				},
			},
			expectedAppState:        v1beta2.UnknownState,
			expectedExecutorState:   map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorPendingState},
			expectedAppMetrics:      metrics{},
			expectedExecutorMetrics: executorMetrics{},
		},
	}

	testFn := func(test testcase, t *testing.T) {
		app.Status.AppState.State = test.oldAppStatus
		app.Status.ExecutorState = test.oldExecutorStatus
		app.Name = test.appName
		app.Status.ExecutionAttempts = 1
		ctrl, _ := newFakeController(app, nil, test.driverPod, test.executorPod)
		_, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Create(app)
		if err != nil {
			t.Fatal(err)
		}
		if test.driverPod != nil {
			ctrl.kubeClient.CoreV1().Pods(app.Namespace).Create(test.driverPod)
		}
		if test.executorPod != nil {
			ctrl.kubeClient.CoreV1().Pods(app.Namespace).Create(test.executorPod)
		}

		err = ctrl.syncSparkApplication(fmt.Sprintf("%s/%s", app.Namespace, app.Name))
		assert.Nil(t, err)
		// Verify application and executor states.
		updatedApp, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Get(app.Name, metav1.GetOptions{})
		assert.Equal(t, test.expectedAppState, updatedApp.Status.AppState.State)
		assert.Equal(t, test.expectedExecutorState, updatedApp.Status.ExecutorState)

		// Validate error message if the driver pod failed.
		if test.driverPod != nil && test.driverPod.Status.Phase == apiv1.PodFailed {
			if len(test.driverPod.Status.ContainerStatuses) > 0 && test.driverPod.Status.ContainerStatuses[0].State.Terminated != nil {
				if test.driverPod.Status.ContainerStatuses[0].State.Terminated.ExitCode != 0 {
					assert.Equal(t, updatedApp.Status.AppState.ErrorMessage,
						fmt.Sprintf("driver container failed with ExitCode: %d, Reason: %s", test.driverPod.Status.ContainerStatuses[0].State.Terminated.ExitCode, test.driverPod.Status.ContainerStatuses[0].State.Terminated.Reason))
				}
			} else {
				assert.Equal(t, updatedApp.Status.AppState.ErrorMessage, "driver container status missing")
			}
		}

		// Verify application metrics.
		assert.Equal(t, test.expectedAppMetrics.runningMetricCount, ctrl.metrics.sparkAppRunningCount.Value(map[string]string{}))
		assert.Equal(t, test.expectedAppMetrics.successMetricCount, fetchCounterValue(ctrl.metrics.sparkAppSuccessCount, map[string]string{}))
		assert.Equal(t, test.expectedAppMetrics.submitMetricCount, fetchCounterValue(ctrl.metrics.sparkAppSubmitCount, map[string]string{}))
		assert.Equal(t, test.expectedAppMetrics.failedMetricCount, fetchCounterValue(ctrl.metrics.sparkAppFailureCount, map[string]string{}))

		// Verify executor metrics.
		assert.Equal(t, test.expectedExecutorMetrics.runningMetricCount, ctrl.metrics.sparkAppExecutorRunningCount.Value(map[string]string{}))
		assert.Equal(t, test.expectedExecutorMetrics.successMetricCount, fetchCounterValue(ctrl.metrics.sparkAppExecutorSuccessCount, map[string]string{}))
		assert.Equal(t, test.expectedExecutorMetrics.failedMetricCount, fetchCounterValue(ctrl.metrics.sparkAppExecutorFailureCount, map[string]string{}))
	}

	for _, test := range testcases {
		testFn(test, t)
	}
}

func TestSyncSparkApplication_ApplicationExpired(t *testing.T) {
	os.Setenv(kubernetesServiceHostEnvVar, "localhost")
	os.Setenv(kubernetesServicePortEnvVar, "443")

	appName := "foo"
	driverPodName := appName + "-driver"

	now := time.Now()
	terminatiomTime := now.Add(-2 * time.Second)
	app := &v1beta2.SparkApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: "test",
		},
		Spec: v1beta2.SparkApplicationSpec{
			RestartPolicy: v1beta2.RestartPolicy{
				Type: v1beta2.Never,
			},
			TimeToLiveSeconds: int64ptr(1),
		},
		Status: v1beta2.SparkApplicationStatus{
			AppState: v1beta2.ApplicationState{
				State:        v1beta2.CompletedState,
				ErrorMessage: "",
			},
			DriverInfo: v1beta2.DriverInfo{
				PodName: driverPodName,
			},
			TerminationTime: metav1.Time{
				Time: terminatiomTime,
			},
			ExecutorState: map[string]v1beta2.ExecutorState{"exec-1": v1beta2.ExecutorCompletedState},
		},
	}

	ctrl, _ := newFakeController(app, nil)
	_, err := ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Create(app)
	if err != nil {
		t.Fatal(err)
	}
	err = ctrl.syncSparkApplication(fmt.Sprintf("%s/%s", app.Namespace, app.Name))
	assert.Nil(t, err)

	_, err = ctrl.crdClient.SparkoperatorV1beta2().SparkApplications(app.Namespace).Get(app.Name, metav1.GetOptions{})
	assert.True(t, apiErrors.IsNotFound(err))
}

func TestHasRetryIntervalPassed(t *testing.T) {
	// Failure cases.
	assert.False(t, hasRetryIntervalPassed(nil, 3, metav1.Time{Time: metav1.Now().Add(-100 * time.Second)}))
	assert.False(t, hasRetryIntervalPassed(int64ptr(5), 0, metav1.Time{Time: metav1.Now().Add(-100 * time.Second)}))
	assert.False(t, hasRetryIntervalPassed(int64ptr(5), 3, metav1.Time{}))
	// Not enough time passed.
	assert.False(t, hasRetryIntervalPassed(int64ptr(50), 3, metav1.Time{Time: metav1.Now().Add(-100 * time.Second)}))
	assert.True(t, hasRetryIntervalPassed(int64ptr(50), 3, metav1.Time{Time: metav1.Now().Add(-151 * time.Second)}))
}

func int32ptr(n int32) *int32 {
	return &n
}

func int64ptr(n int64) *int64 {
	return &n
}

func stringptr(v string) *string {
	return &v
}
