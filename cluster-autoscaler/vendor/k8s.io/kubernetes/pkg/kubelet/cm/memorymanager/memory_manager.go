/*
Copyright 2020 The Kubernetes Authors.

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

package memorymanager

import (
	"fmt"
	"sync"

	cadvisorapi "github.com/google/cadvisor/info/v1"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"
	corev1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

// memoryManagerStateFileName is the file name where memory manager stores its state
const memoryManagerStateFileName = "memory_manager_state"

// ActivePodsFunc is a function that returns a list of active pods
type ActivePodsFunc func() []*v1.Pod

type runtimeService interface {
	UpdateContainerResources(id string, resources *runtimeapi.LinuxContainerResources) error
}

type sourcesReadyStub struct{}

func (s *sourcesReadyStub) AddSource(source string) {}
func (s *sourcesReadyStub) AllReady() bool          { return true }

// Manager interface provides methods for Kubelet to manage pod memory.
type Manager interface {
	// Start is called during Kubelet initialization.
	Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error

	// AddContainer adds the mapping between container ID to pod UID and the container name
	// The mapping used to remove the memory allocation during the container removal
	AddContainer(p *v1.Pod, c *v1.Container, containerID string)

	// Allocate is called to pre-allocate memory resources during Pod admission.
	// This must be called at some point prior to the AddContainer() call for a container, e.g. at pod admission time.
	Allocate(pod *v1.Pod, container *v1.Container) error

	// RemoveContainer is called after Kubelet decides to kill or delete a
	// container. After this call, any memory allocated to the container is freed.
	RemoveContainer(containerID string) error

	// State returns a read-only interface to the internal memory manager state.
	State() state.Reader

	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(*v1.Pod, *v1.Container) map[string][]topologymanager.TopologyHint

	// GetPodTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetPodTopologyHints(*v1.Pod) map[string][]topologymanager.TopologyHint

	// GetMemoryNUMANodes provides NUMA nodes that are used to allocate the container memory
	GetMemoryNUMANodes(pod *v1.Pod, container *v1.Container) sets.Int
}

type manager struct {
	sync.Mutex
	policy Policy

	// state allows to restore information regarding memory allocation for guaranteed pods
	// in the case of the kubelet restart
	state state.State

	// containerRuntime is the container runtime service interface needed
	// to make UpdateContainerResources() calls against the containers.
	containerRuntime runtimeService

	// activePods is a method for listing active pods on the node
	// so all the containers can be updated during call to the removeStaleState.
	activePods ActivePodsFunc

	// podStatusProvider provides a method for obtaining pod statuses
	// and the containerID of their containers
	podStatusProvider status.PodStatusProvider

	// containerMap provides a mapping from (pod, container) -> containerID
	// for all containers a pod
	containerMap containermap.ContainerMap

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// stateFileDirectory holds the directory where the state file for checkpoints is held.
	stateFileDirectory string
}

var _ Manager = &manager{}

// NewManager returns new instance of the memory manager
func NewManager(policyName string, machineInfo *cadvisorapi.MachineInfo, nodeAllocatableReservation v1.ResourceList, reservedMemory []kubeletconfig.MemoryReservation, stateFileDirectory string, affinity topologymanager.Store) (Manager, error) {
	var policy Policy

	switch policyType(policyName) {

	case policyTypeNone:
		policy = NewPolicyNone()

	case policyTypeStatic:
		systemReserved, err := getSystemReservedMemory(machineInfo, nodeAllocatableReservation, reservedMemory)
		if err != nil {
			return nil, err
		}

		policy, err = NewPolicyStatic(machineInfo, systemReserved, affinity)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown policy: \"%s\"", policyName)
	}

	manager := &manager{
		policy:             policy,
		stateFileDirectory: stateFileDirectory,
	}
	manager.sourcesReady = &sourcesReadyStub{}
	return manager, nil
}

// Start starts the memory manager under the kubelet and calls policy start
func (m *manager) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	klog.Infof("[memorymanager] starting with %s policy", m.policy.Name())
	m.sourcesReady = sourcesReady
	m.activePods = activePods
	m.podStatusProvider = podStatusProvider
	m.containerRuntime = containerRuntime
	m.containerMap = initialContainers

	stateImpl, err := state.NewCheckpointState(m.stateFileDirectory, memoryManagerStateFileName, m.policy.Name())
	if err != nil {
		klog.Errorf("[memorymanager] could not initialize checkpoint manager: %v, please drain node and remove policy state file", err)
		return err
	}
	m.state = stateImpl

	err = m.policy.Start(m.state)
	if err != nil {
		klog.Errorf("[memorymanager] policy start error: %v", err)
		return err
	}

	return nil
}

// AddContainer saves the value of requested memory for the guaranteed pod under the state and set memory affinity according to the topolgy manager
func (m *manager) AddContainer(pod *v1.Pod, container *v1.Container, containerID string) {
	m.Lock()
	defer m.Unlock()

	m.containerMap.Add(string(pod.UID), container.Name, containerID)
}

// GetMemory provides NUMA nodes that used to allocate the container memory
func (m *manager) GetMemoryNUMANodes(pod *v1.Pod, container *v1.Container) sets.Int {
	// Get NUMA node affinity of blocks assigned to the container during Allocate()
	numaNodes := sets.NewInt()
	for _, block := range m.state.GetMemoryBlocks(string(pod.UID), container.Name) {
		for _, nodeID := range block.NUMAAffinity {
			// avoid nodes duplication when hugepages and memory blocks pinned to the same NUMA node
			numaNodes.Insert(nodeID)
		}
	}

	if numaNodes.Len() == 0 {
		klog.V(5).Infof("No allocation is available for (Pod: %s, Container: %s)", pod.Name, container.Name)
		return nil
	}

	klog.Infof("(Pod: %s, Container: %s) memory affinity is %v", pod.Name, container.Name, numaNodes)
	return numaNodes
}

// Allocate is called to pre-allocate memory resources during Pod admission.
func (m *manager) Allocate(pod *v1.Pod, container *v1.Container) error {
	// Garbage collect any stranded resources before allocation
	m.removeStaleState()

	m.Lock()
	defer m.Unlock()

	// Call down into the policy to assign this container memory if required.
	if err := m.policy.Allocate(m.state, pod, container); err != nil {
		klog.Errorf("[memorymanager] Allocate error: %v", err)
		return err
	}
	return nil
}

// RemoveContainer removes the container from the state
func (m *manager) RemoveContainer(containerID string) error {
	m.Lock()
	defer m.Unlock()

	// if error appears it means container entry already does not exist under the container map
	podUID, containerName, err := m.containerMap.GetContainerRef(containerID)
	if err != nil {
		klog.Warningf("[memorymanager] Failed to get container %s from container map error: %v", containerID, err)
		return nil
	}

	err = m.policyRemoveContainerByRef(podUID, containerName)
	if err != nil {
		klog.Errorf("[memorymanager] RemoveContainer error: %v", err)
		return err
	}

	return nil
}

// State returns the state of the manager
func (m *manager) State() state.Reader {
	return m.state
}

// GetPodTopologyHints returns the topology hints for the topology manager
func (m *manager) GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState()
	// Delegate to active policy
	return m.policy.GetPodTopologyHints(m.state, pod)
}

// GetTopologyHints returns the topology hints for the topology manager
func (m *manager) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState()
	// Delegate to active policy
	return m.policy.GetTopologyHints(m.state, pod, container)
}

// TODO: move the method to the upper level, to re-use it under the CPU and memory managers
func (m *manager) removeStaleState() {
	// Only once all sources are ready do we attempt to remove any stale state.
	// This ensures that the call to `m.activePods()` below will succeed with
	// the actual active pods list.
	if !m.sourcesReady.AllReady() {
		return
	}

	// We grab the lock to ensure that no new containers will grab memory block while
	// executing the code below. Without this lock, its possible that we end up
	// removing state that is newly added by an asynchronous call to
	// AddContainer() during the execution of this code.
	m.Lock()
	defer m.Unlock()

	// Get the list of active pods.
	activePods := m.activePods()

	// Build a list of (podUID, containerName) pairs for all containers in all active Pods.
	activeContainers := make(map[string]map[string]struct{})
	for _, pod := range activePods {
		activeContainers[string(pod.UID)] = make(map[string]struct{})
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			activeContainers[string(pod.UID)][container.Name] = struct{}{}
		}
	}

	// Loop through the MemoryManager state. Remove any state for containers not
	// in the `activeContainers` list built above.
	assignments := m.state.GetMemoryAssignments()
	for podUID := range assignments {
		for containerName := range assignments[podUID] {
			if _, ok := activeContainers[podUID][containerName]; !ok {
				klog.Infof("[memorymanager] removeStaleState: removing (pod %s, container: %s)", podUID, containerName)
				err := m.policyRemoveContainerByRef(podUID, containerName)
				if err != nil {
					klog.Errorf("[memorymanager] removeStaleState: failed to remove (pod %s, container %s), error: %v)", podUID, containerName, err)
				}
			}
		}
	}
}

func (m *manager) policyRemoveContainerByRef(podUID string, containerName string) error {
	err := m.policy.RemoveContainer(m.state, podUID, containerName)
	if err == nil {
		m.containerMap.RemoveByContainerRef(podUID, containerName)
	}

	return err
}

func getTotalMemoryTypeReserved(machineInfo *cadvisorapi.MachineInfo, reservedMemory []kubeletconfig.MemoryReservation) (map[v1.ResourceName]resource.Quantity, error) {
	totalMemoryType := map[v1.ResourceName]resource.Quantity{}

	numaNodes := map[int]bool{}
	for _, numaNode := range machineInfo.Topology {
		numaNodes[numaNode.Id] = true
	}

	for _, reservation := range reservedMemory {
		if !numaNodes[int(reservation.NumaNode)] {
			return nil, fmt.Errorf("the reserved memory configuration references a NUMA node %d that does not exist on this machine", reservation.NumaNode)
		}

		for resourceName, q := range reservation.Limits {
			if value, ok := totalMemoryType[resourceName]; ok {
				q.Add(value)
			}
			totalMemoryType[resourceName] = q
		}
	}

	return totalMemoryType, nil
}

func validateReservedMemory(machineInfo *cadvisorapi.MachineInfo, nodeAllocatableReservation v1.ResourceList, reservedMemory []kubeletconfig.MemoryReservation) error {
	totalMemoryType, err := getTotalMemoryTypeReserved(machineInfo, reservedMemory)
	if err != nil {
		return err
	}

	commonMemoryTypeSet := make(map[v1.ResourceName]bool)
	for resourceType := range totalMemoryType {
		commonMemoryTypeSet[resourceType] = true
	}

	for resourceType := range nodeAllocatableReservation {
		if !(corev1helper.IsHugePageResourceName(resourceType) || resourceType == v1.ResourceMemory) {
			continue
		}
		commonMemoryTypeSet[resourceType] = true
	}

	for resourceType := range commonMemoryTypeSet {
		nodeAllocatableMemory := resource.NewQuantity(0, resource.DecimalSI)
		if memValue, set := nodeAllocatableReservation[resourceType]; set {
			nodeAllocatableMemory.Add(memValue)
		}

		reservedMemory := resource.NewQuantity(0, resource.DecimalSI)
		if memValue, set := totalMemoryType[resourceType]; set {
			reservedMemory.Add(memValue)
		}

		if !(*nodeAllocatableMemory).Equal(*reservedMemory) {
			return fmt.Errorf("the total amount %q of type %q is not equal to the value %q determined by Node Allocatable feature", reservedMemory.String(), resourceType, nodeAllocatableMemory.String())
		}
	}

	return nil
}

func convertReserved(machineInfo *cadvisorapi.MachineInfo, reservedMemory []kubeletconfig.MemoryReservation) (systemReservedMemory, error) {
	reservedMemoryConverted := make(map[int]map[v1.ResourceName]uint64)
	for _, node := range machineInfo.Topology {
		reservedMemoryConverted[node.Id] = make(map[v1.ResourceName]uint64)
	}

	for _, reservation := range reservedMemory {
		for resourceName, q := range reservation.Limits {
			val, success := q.AsInt64()
			if !success {
				return nil, fmt.Errorf("could not covert a variable of type Quantity to int64")
			}
			reservedMemoryConverted[int(reservation.NumaNode)][resourceName] = uint64(val)
		}
	}

	return reservedMemoryConverted, nil
}

func getSystemReservedMemory(machineInfo *cadvisorapi.MachineInfo, nodeAllocatableReservation v1.ResourceList, reservedMemory []kubeletconfig.MemoryReservation) (systemReservedMemory, error) {
	if err := validateReservedMemory(machineInfo, nodeAllocatableReservation, reservedMemory); err != nil {
		return nil, err
	}

	reservedMemoryConverted, err := convertReserved(machineInfo, reservedMemory)
	if err != nil {
		return nil, err
	}

	return reservedMemoryConverted, nil
}
