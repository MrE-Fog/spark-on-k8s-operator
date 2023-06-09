/*
Copyright 2018 Google LLC

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
	"encoding/json"
	"fmt"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/policy"

	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/apis/sparkoperator.k8s.io/v1beta2"
	"github.com/GoogleCloudPlatform/spark-on-k8s-operator/pkg/config"
)

// Helper method to create a key with namespace and appName
func createMetaNamespaceKey(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func getAppName(object metav1.Object) (string, bool) {
	appName, ok := object.GetLabels()[config.SparkAppNameLabel]
	return appName, ok
}

func getSparkApplicationID(pod *apiv1.Pod) string {
	return pod.Labels[config.SparkApplicationSelectorLabel]
}

func getDriverPodName(app *v1beta2.SparkApplication) string {
	name := app.Spec.Driver.PodName
	if name != nil && len(*name) > 0 {
		return *name
	}

	sparkConf := app.Spec.SparkConf
	if sparkConf[config.SparkDriverPodNameKey] != "" {
		return sparkConf[config.SparkDriverPodNameKey]
	}

	return fmt.Sprintf("%s-driver", app.Name)
}

func getDefaultUIServiceName(app *v1beta2.SparkApplication) string {
	return fmt.Sprintf("%s-ui-svc", app.Name)
}

func getDefaultUIIngressName(app *v1beta2.SparkApplication) string {
	return fmt.Sprintf("%s-ui-ingress", app.Name)
}

func getResourceLabels(app *v1beta2.SparkApplication) map[string]string {
	labels := map[string]string{config.SparkAppNameLabel: app.Name}
	if app.Status.SubmissionID != "" {
		labels[config.SubmissionIDLabel] = app.Status.SubmissionID
	}
	return labels
}

func podPhaseToExecutorState(podPhase apiv1.PodPhase) v1beta2.ExecutorState {
	switch podPhase {
	case apiv1.PodPending:
		return v1beta2.ExecutorPendingState
	case apiv1.PodRunning:
		return v1beta2.ExecutorRunningState
	case apiv1.PodSucceeded:
		return v1beta2.ExecutorCompletedState
	case apiv1.PodFailed:
		return v1beta2.ExecutorFailedState
	default:
		return v1beta2.ExecutorUnknownState
	}
}

func isExecutorTerminated(executorState v1beta2.ExecutorState) bool {
	return executorState == v1beta2.ExecutorCompletedState || executorState == v1beta2.ExecutorFailedState
}

func isDriverRunning(app *v1beta2.SparkApplication) bool {
	return app.Status.AppState.State == v1beta2.RunningState
}

func getDriverContainerTerminatedState(podStatus apiv1.PodStatus) *apiv1.ContainerStateTerminated {
	for _, c := range podStatus.ContainerStatuses {
		if c.Name == config.SparkDriverContainerName {
			if c.State.Terminated != nil {
				return c.State.Terminated
			}
			return nil
		}
	}
	return nil
}

func podStatusToDriverState(podStatus apiv1.PodStatus) v1beta2.DriverState {
	switch podStatus.Phase {
	case apiv1.PodPending:
		return v1beta2.DriverPendingState
	case apiv1.PodRunning:
		state := getDriverContainerTerminatedState(podStatus)
		if state != nil {
			if state.ExitCode == 0 {
				return v1beta2.DriverCompletedState
			}
			return v1beta2.DriverFailedState
		}
		return v1beta2.DriverRunningState
	case apiv1.PodSucceeded:
		return v1beta2.DriverCompletedState
	case apiv1.PodFailed:
		state := getDriverContainerTerminatedState(podStatus)
		if state != nil && state.ExitCode == 0 {
			return v1beta2.DriverCompletedState
		}
		return v1beta2.DriverFailedState
	default:
		return v1beta2.DriverUnknownState
	}
}

func hasDriverTerminated(driverState v1beta2.DriverState) bool {
	return driverState == v1beta2.DriverCompletedState || driverState == v1beta2.DriverFailedState
}

func driverStateToApplicationState(driverState v1beta2.DriverState) v1beta2.ApplicationStateType {
	switch driverState {
	case v1beta2.DriverPendingState:
		return v1beta2.SubmittedState
	case v1beta2.DriverCompletedState:
		return v1beta2.SucceedingState
	case v1beta2.DriverFailedState:
		return v1beta2.FailingState
	case v1beta2.DriverRunningState:
		return v1beta2.RunningState
	default:
		return v1beta2.UnknownState
	}
}

func getVolumeFSType(v apiv1.Volume) (policy.FSType, error) {
	switch {
	case v.HostPath != nil:
		return policy.HostPath, nil
	case v.EmptyDir != nil:
		return policy.EmptyDir, nil
	case v.PersistentVolumeClaim != nil:
		return policy.PersistentVolumeClaim, nil
	}

	return "", fmt.Errorf("unknown volume type for volume: %#v", v)
}

func printStatus(status *v1beta2.SparkApplicationStatus) (string, error) {
	marshalled, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return "", err
	}
	return string(marshalled), nil
}

func getSubmissionJobName(app *v1beta2.SparkApplication) string {
	return app.Name + "-spark-submit"
}
