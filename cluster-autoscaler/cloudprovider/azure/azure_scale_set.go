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

package azure

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	klog "k8s.io/klog/v2"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
)

var (
	defaultVmssInstancesRefreshPeriod = 5 * time.Minute
	vmssContextTimeout                = 3 * time.Minute
	asyncContextTimeout               = 30 * time.Minute
)

const (
	provisioningStateCreating  string = "Creating"
	provisioningStateDeleting  string = "Deleting"
	provisioningStateFailed    string = "Failed"
	provisioningStateMigrating string = "Migrating"
	provisioningStateSucceeded string = "Succeeded"
	provisioningStateUpdating  string = "Updating"
)

// ScaleSet implements NodeGroup interface.
type ScaleSet struct {
	azureRef
	manager *AzureManager

	minSize int
	maxSize int

	enableForceDelete         bool
	enableDynamicInstanceList bool
	enableDetailedCSEMessage  bool

	// uses Azure Dedicated Host
	dedicatedHost bool

	enableFastDeleteOnFailedProvisioning bool
}

// NewScaleSet creates a new NewScaleSet.
func NewScaleSet(spec *dynamic.NodeGroupSpec, az *AzureManager, dedicatedHost bool) (*ScaleSet, error) {
	scaleSet := &ScaleSet{
		azureRef: azureRef{
			Name: spec.Name,
		},

		minSize: spec.MinSize,
		maxSize: spec.MaxSize,

		manager: az,

		enableForceDelete:         az.config.EnableForceDelete,
		enableDynamicInstanceList: az.config.EnableDynamicInstanceList,
		enableDetailedCSEMessage:  az.config.EnableDetailedCSEMessage,
		dedicatedHost:             dedicatedHost,
	}



	if az.config.EnableDetailedCSEMessage {
		klog.V(2).Infof("enableDetailedCSEMessage: %t", scaleSet.enableDetailedCSEMessage)
	}

	scaleSet.enableFastDeleteOnFailedProvisioning = az.config.EnableFastDeleteOnFailedProvisioning

	return scaleSet, nil
}

// MinSize returns minimum size of the node group.
func (scaleSet *ScaleSet) MinSize() int {
	return scaleSet.minSize
}

// Exist checks if the node group really exists on the cloud provider side. Allows to tell the
// theoretical node group from the real one.
func (scaleSet *ScaleSet) Exist() bool {
	return true
}

// Create creates the node group on the cloud provider side.
func (scaleSet *ScaleSet) Create() (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrAlreadyExist
}

// Delete deletes the node group on the cloud provider side.
// This will be executed only for autoprovisioned node groups, once their size drops to 0.
func (scaleSet *ScaleSet) Delete() error {
	return cloudprovider.ErrNotImplemented
}

// Autoprovisioned returns true if the node group is autoprovisioned.
func (scaleSet *ScaleSet) Autoprovisioned() bool {
	return false
}

// GetOptions returns NodeGroupAutoscalingOptions that should be used for this particular
// NodeGroup. Returning a nil will result in using default options.
func (scaleSet *ScaleSet) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	template, err := scaleSet.getVMSS()
	if err != nil {
		klog.Errorf("failed to get information for VMSS: %s", scaleSet.Name)
		// Note: We don't return an error here and instead accept defaults.
		// Every invocation of GetOptions() returns an error if this condition is met:
		// `if err != nil && err != cloudprovider.ErrNotImplemented`
		// The error return value is intended to only capture unimplemented.
		return nil, nil
	}
	return scaleSet.manager.GetScaleSetOptions(*template.Name, defaults), nil
}

// MaxSize returns maximum size of the node group.
func (scaleSet *ScaleSet) MaxSize() int {
	return scaleSet.maxSize
}

func (scaleSet *ScaleSet) getVMSS() (compute.VirtualMachineScaleSet, error) {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	set, rerr := scaleSet.manager.azClient.virtualMachineScaleSetsClient.Get(ctx, scaleSet.manager.config.ResourceGroup, scaleSet.Name)
	if rerr != nil {
		return compute.VirtualMachineScaleSet{}, rerr.Error()
	}

	return set, nil
}

func (scaleSet *ScaleSet) getCurSize() (int64, *GetVMSSFailedError) {
	set, err := scaleSet.getVMSS()
	if err != nil {
		klog.Errorf("failed to get information for VMSS: %s, error: %v", scaleSet.Name, err)
		return -1, newGetVMSSFailedError(err, true)
	}

	if set.Sku == nil || set.Sku.Capacity == nil {
		return -1, newGetVMSSFailedError(fmt.Errorf("VMSS %s has no capacity information", scaleSet.Name), false)
	}

	return *set.Sku.Capacity, nil
}

// getScaleSetSize gets Scale Set size.
func (scaleSet *ScaleSet) getScaleSetSize() (int64, error) {
	// First, get the current size of the ScaleSet
	size, getVMSSError := scaleSet.getCurSize()
	if size == -1 || getVMSSError != nil {
		klog.V(3).Infof("getScaleSetSize: either size is -1 (actual: %d) or error exists (actual err:%v)", size, getVMSSError.error)
		return size, getVMSSError.error
	}
	return size, nil
}

// waitForCreateOrUpdate waits for the outcome of VMSS capacity update initiated via CreateOrUpdateAsync.
func (scaleSet *ScaleSet) waitForCreateOrUpdateInstances(future *azure.Future) {
	var err error

	defer func() {
		if err != nil {
			klog.Errorf("Failed to update the capacity for vmss %s with error %v", scaleSet.Name, err)
		}
	}()

	ctx, cancel := getContextWithTimeout(asyncContextTimeout)
	defer cancel()

	klog.V(3).Infof("Calling virtualMachineScaleSetsClient.WaitForCreateOrUpdateResult(%s)", scaleSet.Name)
	httpResponse, err := scaleSet.manager.azClient.virtualMachineScaleSetsClient.WaitForCreateOrUpdateResult(ctx, future, scaleSet.manager.config.ResourceGroup)

	isSuccess, err := isSuccessHTTPResponse(httpResponse, err)
	if isSuccess {
		klog.V(3).Infof("waitForCreateOrUpdateInstances(%s) success", scaleSet.Name)
		return
	}

	klog.Errorf("waitForCreateOrUpdateInstances(%s) failed, err: %v", scaleSet.Name, err)
}

// setScaleSetSize sets ScaleSet size.
func (scaleSet *ScaleSet) setScaleSetSize(size int64, delta int) error {
	vmssInfo, err := scaleSet.getVMSS()
	if err != nil {
		klog.Errorf("Failed to get information for VMSS (%q): %v", scaleSet.Name, err)
		return err
	}

	requiredInstances := delta

	// If after reallocating instances we still need more instances or we're just in Delete mode
	// send a scale request
	if requiredInstances > 0 {
		klog.V(3).Infof("Remaining unsatisfied count is %d. Attempting to increase scale set %q "+
			"capacity", requiredInstances, scaleSet.Name)
		err := scaleSet.createOrUpdateInstances(&vmssInfo, size)
		if err != nil {
			klog.Errorf("Failed to increase capacity for scale set %q to %d: %v", scaleSet.Name, requiredInstances, err)
			return err
		}
	}
	return nil
}

// TargetSize returns the current TARGET size of the node group. It is possible that the
// number is different from the number of nodes registered in Kubernetes.
func (scaleSet *ScaleSet) TargetSize() (int, error) {
	size, err := scaleSet.getScaleSetSize()
	return int(size), err
}

// IncreaseSize increases Scale Set size
func (scaleSet *ScaleSet) IncreaseSize(delta int) error {
	if delta <= 0 {
		return fmt.Errorf("size increase must be positive")
	}

	size, err := scaleSet.getScaleSetSize()
	if err != nil {
		return err
	}

	if size == -1 {
		return fmt.Errorf("the scale set %s is under initialization, skipping IncreaseSize", scaleSet.Name)
	}

	if int(size)+delta > scaleSet.MaxSize() {
		return fmt.Errorf("size increase too large - desired:%d max:%d", int(size)+delta, scaleSet.MaxSize())
	}

	return scaleSet.setScaleSetSize(size+int64(delta), delta)
}

// AtomicIncreaseSize is not implemented.
func (scaleSet *ScaleSet) AtomicIncreaseSize(delta int) error {
	return cloudprovider.ErrNotImplemented
}

// GetScaleSetVms returns list of nodes for the given scale set.
func (scaleSet *ScaleSet) GetScaleSetVms() ([]compute.VirtualMachineScaleSetVM, *retry.Error) {
	ctx, cancel := getContextWithTimeout(vmssContextTimeout)
	defer cancel()

	vmList, rerr := scaleSet.manager.azClient.virtualMachineScaleSetVMsClient.List(ctx, scaleSet.manager.config.ResourceGroup,
		scaleSet.Name, string(compute.InstanceViewTypesInstanceView))

	klog.V(4).Infof("GetScaleSetVms: scaleSet.Name: %s, vmList: %v", scaleSet.Name, vmList)

	if rerr != nil {
		klog.Errorf("VirtualMachineScaleSetVMsClient.List failed for %s: %v", scaleSet.Name, rerr)
		return nil, rerr
	}

	return vmList, nil
}

// GetFlexibleScaleSetVms returns list of nodes for flexible scale set.
func (scaleSet *ScaleSet) GetFlexibleScaleSetVms() ([]compute.VirtualMachine, *retry.Error) {
	klog.V(4).Infof("GetScaleSetVms: starts")
	ctx, cancel := getContextWithTimeout(vmssContextTimeout)
	defer cancel()

	// get VMSS info from cache to obtain ID currently scaleSet does not store ID info.
	vmssInfo, err := scaleSet.getVMSS()

	if err != nil {
		klog.Errorf("Failed to get information for VMSS (%q): %v", scaleSet.Name, err)
		var rerr = &retry.Error{
			RawError: err,
		}
		return nil, rerr
	}
	vmList, rerr := scaleSet.manager.azClient.virtualMachinesClient.ListVmssFlexVMsWithoutInstanceView(ctx, *vmssInfo.ID)
	if rerr != nil {
		klog.Errorf("VirtualMachineScaleSetVMsClient.List failed for %s: %v", scaleSet.Name, rerr)
		return nil, rerr
	}
	klog.V(4).Infof("GetFlexibleScaleSetVms: scaleSet.Name: %s, vmList: %v", scaleSet.Name, vmList)
	return vmList, nil
}

// DecreaseTargetSize decreases the target size of the node group. This function
// doesn't permit to delete any existing node and can be used only to reduce the
// request for new nodes that have not been yet fulfilled. Delta should be negative.
// It is assumed that cloud provider will not delete the existing nodes if the size
// when there is an option to just decrease the target.
func (scaleSet *ScaleSet) DecreaseTargetSize(delta int) error {
	// VMSS size should be changed automatically after the Node deletion, hence this operation is not required.
	_, err := scaleSet.getScaleSetSize()
	if err != nil {
		klog.Warningf("DecreaseTargetSize: failed with error: %v", err)
	}
	return err
}

// Belongs returns true if the given node belongs to the NodeGroup.
func (scaleSet *ScaleSet) Belongs(node *apiv1.Node) (bool, error) {
	klog.V(6).Infof("Check if node belongs to this scale set: scaleset:%v, node:%v\n", scaleSet, node)

	ref := &azureRef{
		Name: node.Spec.ProviderID,
	}

	targetAsg, err := scaleSet.manager.GetNodeGroupForInstance(ref)
	if err != nil {
		return false, err
	}
	if targetAsg == nil {
		return false, fmt.Errorf("%s doesn't belong to a known scale set", node.Name)
	}
	if !strings.EqualFold(targetAsg.Id(), scaleSet.Id()) {
		return false, nil
	}
	return true, nil
}

func (scaleSet *ScaleSet) createOrUpdateInstances(vmssInfo *compute.VirtualMachineScaleSet, newSize int64) error {
	if vmssInfo == nil {
		return fmt.Errorf("vmssInfo cannot be nil while increating scaleSet capacity")
	}

	// Update the new capacity.
	vmssInfo.Sku.Capacity = &newSize

	// Compose a new VMSS for updating.
	op := compute.VirtualMachineScaleSet{
		Name:     vmssInfo.Name,
		Sku:      vmssInfo.Sku,
		Location: vmssInfo.Location,
	}

	if vmssInfo.ExtendedLocation != nil {
		op.ExtendedLocation = &compute.ExtendedLocation{
			Name: vmssInfo.ExtendedLocation.Name,
			Type: vmssInfo.ExtendedLocation.Type,
		}

		klog.V(3).Infof("Passing ExtendedLocation information if it is not nil, with Edge Zone name:(%s)", *op.ExtendedLocation.Name)
	}

	ctx, cancel := getContextWithTimeout(vmssContextTimeout)
	defer cancel()
	klog.V(3).Infof("Waiting for virtualMachineScaleSetsClient.CreateOrUpdateAsync(%s)", scaleSet.Name)
	future, rerr := scaleSet.manager.azClient.virtualMachineScaleSetsClient.CreateOrUpdateAsync(ctx, scaleSet.manager.config.ResourceGroup, scaleSet.Name, op)
	if rerr != nil {
		klog.Errorf("virtualMachineScaleSetsClient.CreateOrUpdate for scale set %q failed: %+v", scaleSet.Name, rerr)
		return rerr.Error()
	}


	go scaleSet.waitForCreateOrUpdateInstances(future)
	return nil
}

// DeleteInstances deletes the given instances. All instances must be controlled by the same nodegroup.
func (scaleSet *ScaleSet) DeleteInstances(instances []*azureRef, hasUnregisteredNodes bool) error {
	if len(instances) == 0 {
		return nil
	}

	klog.V(3).Infof("Deleting vmss instances %v", instances)

	commonAsg, err := scaleSet.manager.GetNodeGroupForInstance(instances[0])
	if err != nil {
		return err
	}

	instancesToDelete := []*azureRef{}
	for _, instance := range instances {
		err = scaleSet.verifyNodeGroup(instance, commonAsg.Id())
		if err != nil {
			return err
		}

		instancesToDelete = append(instancesToDelete, instance)
	}

	// nothing to delete
	if len(instancesToDelete) == 0 {
		klog.V(3).Infof("No new instances eligible for deletion, skipping")
		return nil
	}

	instanceIDs := []string{}
	for _, instance := range instancesToDelete {
		instanceID, err := getLastSegment(instance.Name)
		if err != nil {
			klog.Errorf("getLastSegment failed with error: %v", err)
			return err
		}
		instanceIDs = append(instanceIDs, instanceID)
	}

	requiredIds := &compute.VirtualMachineScaleSetVMInstanceRequiredIDs{
		InstanceIds: &instanceIDs,
	}

	ctx, cancel := getContextWithTimeout(vmssContextTimeout)
	defer cancel()

	future, rerr := scaleSet.deleteInstances(ctx, requiredIds, commonAsg.Id())
	if rerr != nil {
		klog.Errorf("virtualMachineScaleSetsClient.DeleteInstancesAsync for instances %v for %s failed: %+v", requiredIds.InstanceIds, scaleSet.Name, rerr)
		return rerr.Error()
	}


	go scaleSet.waitForDeleteInstances(future, requiredIds)
	return nil
}

func (scaleSet *ScaleSet) waitForDeleteInstances(future *azure.Future, requiredIds *compute.VirtualMachineScaleSetVMInstanceRequiredIDs) {
	ctx, cancel := getContextWithTimeout(asyncContextTimeout)

	defer cancel()
	klog.V(3).Infof("Calling virtualMachineScaleSetsClient.WaitForDeleteInstancesResult(%v) for %s", requiredIds.InstanceIds, scaleSet.Name)
	httpResponse, err := scaleSet.manager.azClient.virtualMachineScaleSetsClient.WaitForDeleteInstancesResult(ctx, future, scaleSet.manager.config.ResourceGroup)
	isSuccess, err := isSuccessHTTPResponse(httpResponse, err)
	if isSuccess {
		klog.V(3).Infof(".WaitForDeleteInstancesResult(%v) for %s success", requiredIds.InstanceIds, scaleSet.Name)
		// Note: cache refresh logic removed as part of decaching effort
		return
	}
	klog.Errorf("WaitForDeleteInstancesResult(%v) for %s failed with error: %v", requiredIds.InstanceIds, scaleSet.Name, err)
}

// DeleteNodes deletes the nodes from the group.
func (scaleSet *ScaleSet) DeleteNodes(nodes []*apiv1.Node) error {
	klog.V(8).Infof("Delete nodes requested: %q\n", nodes)
	size, err := scaleSet.getScaleSetSize()
	if err != nil {
		return err
	}

	if int(size) <= scaleSet.MinSize() {
		return fmt.Errorf("min size reached, nodes will not be deleted")
	}

	// Distinguish between unregistered node deletion and normal node deletion
	refs := make([]*azureRef, 0, len(nodes))
	hasUnregisteredNodes := false
	unregisteredRefs := make([]*azureRef, 0, len(nodes))

	for _, node := range nodes {
		belongs, err := scaleSet.Belongs(node)
		if err != nil {
			return err
		}

		if belongs != true {
			return fmt.Errorf("%s belongs to a different asg than %s", node.Name, scaleSet.Id())
		}

		if node.Annotations[cloudprovider.FakeNodeReasonAnnotation] == cloudprovider.FakeNodeUnregistered {
			hasUnregisteredNodes = true
		}
		ref := &azureRef{
			Name: node.Spec.ProviderID,
		}

		if node.Annotations[cloudprovider.FakeNodeReasonAnnotation] == cloudprovider.FakeNodeUnregistered {
			klog.V(5).Infof("Node: %s type is unregistered..Appending to the unregistered list", node.Name)
			unregisteredRefs = append(unregisteredRefs, ref)
		} else {
			refs = append(refs, ref)
		}
	}

	if len(unregisteredRefs) > 0 {
		klog.V(3).Infof("Removing unregisteredNodes: %v", unregisteredRefs)
		return scaleSet.DeleteInstances(unregisteredRefs, true)
	}

	return scaleSet.DeleteInstances(refs, hasUnregisteredNodes)
}

// Id returns ScaleSet id.
func (scaleSet *ScaleSet) Id() string {
	return scaleSet.Name
}

// Debug returns a debug string for the Scale Set.
func (scaleSet *ScaleSet) Debug() string {
	return fmt.Sprintf("%s (%d:%d)", scaleSet.Id(), scaleSet.MinSize(), scaleSet.MaxSize())
}

// TemplateNodeInfo returns a node template for this scale set.
func (scaleSet *ScaleSet) TemplateNodeInfo() (*schedulerframework.NodeInfo, error) {
	template, err := scaleSet.getVMSS()
	if err != nil {
		return nil, err
	}

	inputLabels := map[string]string{}
	inputTaints := ""
	node, err := buildNodeFromTemplate(scaleSet.Name, inputLabels, inputTaints, template, scaleSet.manager, scaleSet.enableDynamicInstanceList)

	if err != nil {
		return nil, err
	}

	nodeInfo := schedulerframework.NewNodeInfo(cloudprovider.BuildKubeProxy(scaleSet.Name))
	nodeInfo.SetNode(node)
	return nodeInfo, nil
}

// Nodes returns a list of all nodes that belong to this node group.
func (scaleSet *ScaleSet) Nodes() ([]cloudprovider.Instance, error) {
	orchestrationMode, err := scaleSet.getOrchestrationMode()
	if err != nil {
		klog.Errorf("failed to get information for VMSS: %s, error: %v", scaleSet.Name, err)
		return nil, err
	}

	if orchestrationMode == compute.Flexible {
		if scaleSet.manager.config.EnableVmssFlexNodes {
			vms, rerr := scaleSet.GetFlexibleScaleSetVms()
			if rerr != nil {
				return nil, rerr.Error()
			}
			return buildInstanceCacheForFlex(vms, scaleSet.enableFastDeleteOnFailedProvisioning), nil
		}
		return nil, fmt.Errorf("vmss - %q with Flexible orchestration detected but 'enableVmssFlexNodes' feature flag is turned off", scaleSet.Name)
	} else if orchestrationMode == compute.Uniform {
		vms, rerr := scaleSet.GetScaleSetVms()
		if rerr != nil {
			return nil, rerr.Error()
		}

		instances := []cloudprovider.Instance{}
		// Note that the GetScaleSetVms() results is not used directly because for the List endpoint,
		// their resource ID format is not consistent with Get endpoint
		for i := range vms {
			// The resource ID is empty string, which indicates the instance may be in deleting state.
			if *vms[i].ID == "" {
				continue
			}
			resourceID, err := convertResourceGroupNameToLower(*vms[i].ID)
			if err != nil {
				// This shouldn't happen. Log a warning message for tracking.
				klog.Warningf("Nodes: convertResourceGroupNameToLower failed with error: %v", err)
				continue
			}

			instances = append(instances, cloudprovider.Instance{
				Id:     azurePrefix + resourceID,
				Status: scaleSet.instanceStatusFromVM(&vms[i]),
			})
		}
		return instances, nil
	}

	return nil, fmt.Errorf("failed to determine orchestration mode for vmss %q", scaleSet.Name)
}


// Note that the GetScaleSetVms() results is not used directly because for the List endpoint,
// their resource ID format is not consistent with Get endpoint
// buildInstanceCacheForFlex used by orchestrationMode == compute.Flexible
func buildInstanceCacheForFlex(vms []compute.VirtualMachine, enableFastDeleteOnFailedProvisioning bool) []cloudprovider.Instance {
	var instances []cloudprovider.Instance
	for _, vm := range vms {
		powerState := vmPowerStateRunning
		if vm.InstanceView != nil && vm.InstanceView.Statuses != nil {
			powerState = vmPowerStateFromStatuses(*vm.InstanceView.Statuses)
		}
		addVMToCache(&instances, vm.ID, vm.ProvisioningState, powerState, enableFastDeleteOnFailedProvisioning)
	}

	return instances
}

// instanceStatusFromVM converts the VM provisioning state to cloudprovider.InstanceStatus.
func (scaleSet *ScaleSet) instanceStatusFromVM(vm *compute.VirtualMachineScaleSetVM) *cloudprovider.InstanceStatus {
	powerState := vmPowerStateRunning
	if vm.InstanceView != nil && vm.InstanceView.Statuses != nil {
		powerState = vmPowerStateFromStatuses(*vm.InstanceView.Statuses)
	}

	status := &cloudprovider.InstanceStatus{}
	switch *vm.ProvisioningState {
	case string(compute.GalleryProvisioningStateDeleting):
		status.State = cloudprovider.InstanceDeleting
	case string(compute.GalleryProvisioningStateCreating):
		status.State = cloudprovider.InstanceCreating
	case string(compute.GalleryProvisioningStateFailed):
		status.State = cloudprovider.InstanceRunning

		klog.V(3).Infof("VM %s reports failed provisioning state with power state: %s, eligible for fast delete: %s", to.String(vm.ID), powerState, strconv.FormatBool(scaleSet.enableFastDeleteOnFailedProvisioning))
		if scaleSet.enableFastDeleteOnFailedProvisioning {
			// Provisioning can fail both during instance creation or after the instance is running.
			// Per https://learn.microsoft.com/en-us/azure/virtual-machines/states-billing#provisioning-states,
			// ProvisioningState represents the most recent provisioning state, therefore only report
			// InstanceCreating errors when the power state indicates the instance has not yet started running
			if !isRunningVmPowerState(powerState) {
				// This fast deletion relies on the fact that InstanceCreating + ErrorInfo will subsequently trigger a deletion.
				// Could be revisited to rely on something more stable/explicit.
				status.State = cloudprovider.InstanceCreating
				status.ErrorInfo = &cloudprovider.InstanceErrorInfo{
					ErrorClass:   cloudprovider.OutOfResourcesErrorClass,
					ErrorCode:    "provisioning-state-failed",
					ErrorMessage: "Azure failed to provision a node for this node group",
				}
			} else {
				status.State = cloudprovider.InstanceRunning
			}
		}
	default:
		status.State = cloudprovider.InstanceRunning
	}

	// Add vmssCSE Provisioning Failed Message in error info body for vmssCSE Extensions if enableDetailedCSEMessage is true
	if scaleSet.enableDetailedCSEMessage && vm.InstanceView != nil {
		if err, failed := scaleSet.cseErrors(vm.InstanceView.Extensions); failed {
			klog.V(3).Infof("VM %s reports CSE failure: %v, with provisioning state %s, power state %s", to.String(vm.ID), err, to.String(vm.ProvisioningState), powerState)
			status.State = cloudprovider.InstanceCreating
			errorInfo := &cloudprovider.InstanceErrorInfo{
				ErrorClass:   cloudprovider.OtherErrorClass,
				ErrorCode:    vmssExtensionProvisioningFailed,
				ErrorMessage: fmt.Sprintf("%s: %v", to.String(vm.Name), err),
			}
			status.ErrorInfo = errorInfo
		}
	}

	return status
}

// addVMToCache used by orchestrationMode == compute.Flexible
func addVMToCache(instances *[]cloudprovider.Instance, id, provisioningState *string, powerState string, enableFastDeleteOnFailedProvisioning bool) {
	// The resource ID is empty string, which indicates the instance may be in deleting state.
	if len(*id) == 0 {
		return
	}

	resourceID, err := convertResourceGroupNameToLower(*id)
	if err != nil {
		// This shouldn't happen. Log a warning message for tracking.
		klog.Warningf("buildInstanceCache.convertResourceGroupNameToLower failed with error: %v", err)
		return
	}

	*instances = append(*instances, cloudprovider.Instance{
		Id:     azurePrefix + resourceID,
		Status: instanceStatusFromProvisioningStateAndPowerState(resourceID, provisioningState, powerState, enableFastDeleteOnFailedProvisioning),
	})
}

// instanceStatusFromProvisioningStateAndPowerState converts the VM provisioning state to cloudprovider.InstanceStatus
// instanceStatusFromProvisioningStateAndPowerState used by orchestrationMode == compute.Flexible
// Suggestion: reunify this with scaleSet.instanceStatusFromVM()
func instanceStatusFromProvisioningStateAndPowerState(resourceID string, provisioningState *string, powerState string, enableFastDeleteOnFailedProvisioning bool) *cloudprovider.InstanceStatus {
	if provisioningState == nil {
		return nil
	}

	klog.V(5).Infof("Getting vm instance provisioning state %s for %s", *provisioningState, resourceID)

	status := &cloudprovider.InstanceStatus{}
	switch *provisioningState {
	case provisioningStateDeleting:
		status.State = cloudprovider.InstanceDeleting
	case provisioningStateCreating:
		status.State = cloudprovider.InstanceCreating
	case provisioningStateFailed:
		status.State = cloudprovider.InstanceRunning

		if enableFastDeleteOnFailedProvisioning {
			// Provisioning can fail both during instance creation or after the instance is running.
			// Per https://learn.microsoft.com/en-us/azure/virtual-machines/states-billing#provisioning-states,
			// ProvisioningState represents the most recent provisioning state, therefore only report
			// InstanceCreating errors when the power state indicates the instance has not yet started running
			if !isRunningVmPowerState(powerState) {
				klog.V(4).Infof("VM %s reports failed provisioning state with non-running power state: %s", resourceID, powerState)
				status.State = cloudprovider.InstanceCreating
				status.ErrorInfo = &cloudprovider.InstanceErrorInfo{
					ErrorClass:   cloudprovider.OutOfResourcesErrorClass,
					ErrorCode:    "provisioning-state-failed",
					ErrorMessage: "Azure failed to provision a node for this node group",
				}
			} else {
				klog.V(5).Infof("VM %s reports a failed provisioning state but is running (%s)", resourceID, powerState)
				status.State = cloudprovider.InstanceRunning
			}
		}
	default:
		status.State = cloudprovider.InstanceRunning
	}

	return status
}

func isSpot(vmss *compute.VirtualMachineScaleSet) bool {
	return vmss != nil && vmss.VirtualMachineScaleSetProperties != nil &&
		vmss.VirtualMachineScaleSetProperties.VirtualMachineProfile != nil &&
		vmss.VirtualMachineScaleSetProperties.VirtualMachineProfile.Priority == compute.Spot
}


func (scaleSet *ScaleSet) getOrchestrationMode() (compute.OrchestrationMode, error) {
	vmss, err := scaleSet.getVMSS()
	if err != nil {
		klog.Errorf("failed to get information for VMSS: %s, error: %v", scaleSet.Name, err)
		return "", err
	}
	return vmss.OrchestrationMode, nil
}

func (scaleSet *ScaleSet) cseErrors(extensions *[]compute.VirtualMachineExtensionInstanceView) ([]string, bool) {
	var errs []string
	failed := false
	if extensions != nil {
		for _, extension := range *extensions {
			if strings.EqualFold(to.String(extension.Name), vmssCSEExtensionName) && extension.Statuses != nil {
				for _, status := range *extension.Statuses {
					if status.Level == "Error" {
						errs = append(errs, to.String(status.Message))
						failed = true
					}
				}
			}
		}
	}
	return errs, failed
}

func (scaleSet *ScaleSet) getSKU() string {
	vmssInfo, err := scaleSet.getVMSS()
	if err != nil {
		klog.Errorf("Failed to get information for VMSS (%q): %v", scaleSet.Name, err)
		return ""
	}
	return to.String(vmssInfo.Sku.Name)
}

func (scaleSet *ScaleSet) verifyNodeGroup(instance *azureRef, commonNgID string) error {
	ng, err := scaleSet.manager.GetNodeGroupForInstance(instance)
	if err != nil {
		return err
	}

	if !strings.EqualFold(ng.Id(), commonNgID) {
		return fmt.Errorf("cannot delete instance (%s) which don't belong to the same Scale Set (%q)",
			instance.Name, commonNgID)
	}
	return nil
}

// GetVMSSFailedError is used to differentiate between
// NotFound and other errors
type GetVMSSFailedError struct {
	notFound bool
	error    error
}

func newGetVMSSFailedError(error error, notFound bool) *GetVMSSFailedError {
	return &GetVMSSFailedError{
		error:    error,
		notFound: notFound,
	}
}

func (v *GetVMSSFailedError) Error() string {
	return v.error.Error()
}
