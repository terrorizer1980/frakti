/*
Copyright 2016 The Kubernetes Authors.

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

package hyper

import (
	"fmt"
	"io"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"

	kubeapi "k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"
)

const (
	hyperRuntimeName    = "hyper"
	minimumHyperVersion = "0.6.0"
	secondToNano        = 1e9

	// timeout in second for interacting with hyperd's gRPC API.
	hyperConnectionTimeout = 300 * time.Second
)

// Runtime is the HyperContainer implementation of kubelet runtime API
type Runtime struct {
	client *Client
}

// NewHyperRuntime creates a new Runtime
func NewHyperRuntime(hyperEndpoint string) (*Runtime, error) {
	hyperClient, err := NewClient(hyperEndpoint, hyperConnectionTimeout)
	if err != nil {
		glog.Fatalf("Initialize hyper client failed: %v", err)
		return nil, err
	}

	return &Runtime{client: hyperClient}, nil
}

// Version returns the runtime name, runtime version and runtime API version
func (h *Runtime) Version() (string, string, string, error) {
	version, apiVersion, err := h.client.GetVersion()
	if err != nil {
		glog.Errorf("Get hyper version failed: %v", err)
		return "", "", "", err
	}

	return hyperRuntimeName, version, apiVersion, nil
}

// Status returns the status of the runtime.
func (h *Runtime) Status() (*kubeapi.RuntimeStatus, error) {
	runtimeReady := &kubeapi.RuntimeCondition{
		Type:   proto.String(kubeapi.RuntimeReady),
		Status: proto.Bool(true),
	}
	// Always set networkReady for now.
	// TODO: get real network status when network plugin is enabled.
	networkReady := &kubeapi.RuntimeCondition{
		Type:   proto.String(kubeapi.NetworkReady),
		Status: proto.Bool(true),
	}
	conditions := []*kubeapi.RuntimeCondition{runtimeReady, networkReady}
	if _, _, err := h.client.GetVersion(); err != nil {
		runtimeReady.Status = proto.Bool(false)
		runtimeReady.Reason = proto.String("HyperDaemonNotReady")
		runtimeReady.Message = proto.String(fmt.Sprintf("hyper: failed to get hyper version: %v", err))
	}

	return &kubeapi.RuntimeStatus{Conditions: conditions}, nil
}

// RunPodSandbox creates and starts a pod-level sandbox.
func (h *Runtime) RunPodSandbox(config *kubeapi.PodSandboxConfig) (string, error) {
	userpod, err := buildUserPod(config)
	if err != nil {
		glog.Errorf("Build UserPod for sandbox %q failed: %v", config.String(), err)
		return "", err
	}

	podID, err := h.client.CreatePod(userpod)
	if err != nil {
		glog.Errorf("Create pod for sandbox %q failed: %v", config.String(), err)
		return "", err
	}

	err = h.client.StartPod(podID)
	if err != nil {
		glog.Errorf("Start pod %q failed: %v", podID, err)
		if removeError := h.client.RemovePod(podID); removeError != nil {
			glog.Warningf("Remove pod %q failed: %v", removeError)
		}
		return "", err
	}

	return podID, nil
}

// StopPodSandbox stops the sandbox. If there are any running containers in the
// sandbox, they should be force terminated.
func (h *Runtime) StopPodSandbox(podSandboxID string) error {
	code, cause, err := h.client.StopPod(podSandboxID)
	if err != nil {
		glog.Errorf("Stop pod %s failed, code: %d, cause: %s, error: %v", podSandboxID, code, cause, err)
		return err
	}

	return nil
}

// DeletePodSandbox deletes the sandbox. If there are any running containers in the
// sandbox, they should be force deleted.
func (h *Runtime) DeletePodSandbox(podSandboxID string) error {
	err := h.client.RemovePod(podSandboxID)
	if err != nil {
		glog.Errorf("Remove pod %s failed: %v", podSandboxID, err)
		return err
	}

	return nil
}

// PodSandboxStatus returns the Status of the PodSandbox.
func (h *Runtime) PodSandboxStatus(podSandboxID string) (*kubeapi.PodSandboxStatus, error) {
	info, err := h.client.GetPodInfo(podSandboxID)
	if err != nil {
		glog.Errorf("GetPodInfo for %s failed: %v", podSandboxID, err)
		return nil, err
	}

	state := toPodSandboxState(info.Status.Phase)
	podIP := ""
	if len(info.Status.PodIP) > 0 {
		podIP = info.Status.PodIP[0]
	}

	podName, podNamespace, podUID, attempt, err := parseSandboxName(info.PodName)
	if err != nil {
		glog.Errorf("ParseSandboxName for %s failed: %v", info.PodName, err)
		return nil, err
	}

	podSandboxMetadata := &kubeapi.PodSandboxMetadata{
		Name:      &podName,
		Uid:       &podUID,
		Namespace: &podNamespace,
		Attempt:   &attempt,
	}

	annotations := getAnnotationsFromLabels(info.Spec.Labels)
	kubeletLabels := getKubeletLabels(info.Spec.Labels)
	createdAtNano := info.CreatedAt * secondToNano
	podStatus := &kubeapi.PodSandboxStatus{
		Id:          &podSandboxID,
		Metadata:    podSandboxMetadata,
		State:       &state,
		Network:     &kubeapi.PodSandboxNetworkStatus{Ip: &podIP},
		CreatedAt:   &createdAtNano,
		Labels:      kubeletLabels,
		Annotations: annotations,
	}

	return podStatus, nil
}

// ListPodSandbox returns a list of Sandbox.
func (h *Runtime) ListPodSandbox(filter *kubeapi.PodSandboxFilter) ([]*kubeapi.PodSandbox, error) {
	pods, err := h.client.GetPodList()
	if err != nil {
		glog.Errorf("GetPodList failed: %v", err)
		return nil, err
	}

	items := make([]*kubeapi.PodSandbox, 0, len(pods))
	for _, pod := range pods {
		state := toPodSandboxState(pod.Status)

		podName, podNamespace, podUID, attempt, err := parseSandboxName(pod.PodName)
		if err != nil {
			glog.Errorf("ParseSandboxName for %s failed: %v", pod.PodName, err)
			return nil, err
		}

		if filter != nil {
			if filter.Id != nil && pod.PodID != filter.GetId() {
				continue
			}

			if filter.State != nil && state != filter.GetState() {
				continue
			}

			if filter.LabelSelector != nil && !inMap(filter.LabelSelector, pod.Labels) {
				continue
			}
		}

		podSandboxMetadata := &kubeapi.PodSandboxMetadata{
			Name:      &podName,
			Uid:       &podUID,
			Namespace: &podNamespace,
			Attempt:   &attempt,
		}

		createdAtNano := pod.CreatedAt * secondToNano
		items = append(items, &kubeapi.PodSandbox{
			Id:        &pod.PodID,
			Metadata:  podSandboxMetadata,
			Labels:    pod.Labels,
			State:     &state,
			CreatedAt: &createdAtNano,
		})
	}

	sortByCreatedAt(items)

	return items, nil
}

// ExecSync runs a command in a container synchronously.
func (h *Runtime) ExecSync() error {
	return fmt.Errorf("Not implemented")
}

// Exec execute a command in the container.
func (h *Runtime) Exec(rawContainerID string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.WriteCloser) error {
	return fmt.Errorf("Not implemented")
}

// Attach prepares a streaming endpoint to attach to a running container.
func (h *Runtime) Attach() error {
	return fmt.Errorf("Not implemented")
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox.
func (h *Runtime) PortForward() error {
	return fmt.Errorf("Not implemented")
}

// UpdateRuntimeConfig updates runtime configuration if specified
func (h *Runtime) UpdateRuntimeConfig(runtimeConfig *kubeapi.RuntimeConfig) error {
	return nil
}
