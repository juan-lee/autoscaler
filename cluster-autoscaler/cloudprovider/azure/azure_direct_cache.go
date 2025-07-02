/*
Copyright 2025 The Kubernetes Authors.

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
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/skewer"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/klog/v2"
	providerazureconsts "sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// DirectResourceCache implements ResourceCache interface without caching,
// making direct API calls for all operations.
type DirectResourceCache struct {
	azClient             *azClient
	config               *Config
	registeredNodeGroups []cloudprovider.NodeGroup
	mutex                sync.RWMutex // Only protects registeredNodeGroups
}

// newDirectResourceCache creates a new DirectResourceCache instance.
func newDirectResourceCache(client *azClient, config *Config) *DirectResourceCache {
	return &DirectResourceCache{
		azClient:             client,
		config:               config,
		registeredNodeGroups: make([]cloudprovider.NodeGroup, 0),
	}
}

// Register registers a node group. Only tracks registered groups, no caching.
func (d *DirectResourceCache) Register(nodeGroup cloudprovider.NodeGroup) bool {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	for i := range d.registeredNodeGroups {
		if existing := d.registeredNodeGroups[i]; strings.EqualFold(existing.Id(), nodeGroup.Id()) {
			if existing.MinSize() == nodeGroup.MinSize() && existing.MaxSize() == nodeGroup.MaxSize() {
				return false
			}
			d.registeredNodeGroups[i] = nodeGroup
			klog.V(4).Infof("DirectCache: Node group %q updated", nodeGroup.Id())
			return true
		}
	}

	klog.V(4).Infof("DirectCache: Registering Node Group %q", nodeGroup.Id())
	d.registeredNodeGroups = append(d.registeredNodeGroups, nodeGroup)
	return true
}

// Unregister removes a node group from tracking.
func (d *DirectResourceCache) Unregister(nodeGroup cloudprovider.NodeGroup) bool {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	updated := make([]cloudprovider.NodeGroup, 0, len(d.registeredNodeGroups))
	changed := false
	for _, existing := range d.registeredNodeGroups {
		if strings.EqualFold(existing.Id(), nodeGroup.Id()) {
			klog.V(1).Infof("DirectCache: Unregistered node group %s", nodeGroup.Id())
			changed = true
			continue
		}
		updated = append(updated, existing)
	}
	d.registeredNodeGroups = updated
	return changed
}

// getRegisteredNodeGroups returns the list of registered node groups.
func (d *DirectResourceCache) getRegisteredNodeGroups() []cloudprovider.NodeGroup {
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.registeredNodeGroups
}

// getScaleSets makes a direct API call to fetch VMSS without caching.
func (d *DirectResourceCache) getScaleSets() map[string]compute.VirtualMachineScaleSet {
	ctx, cancel := getContextWithTimeout(vmssContextTimeout)
	defer cancel()

	result, err := d.azClient.virtualMachineScaleSetsClient.List(ctx, d.config.ResourceGroup)
	if err != nil {
		klog.Errorf("DirectCache: VirtualMachineScaleSetsClient.List failed: %v", err)
		return make(map[string]compute.VirtualMachineScaleSet)
	}

	sets := make(map[string]compute.VirtualMachineScaleSet)
	for _, vmss := range result {
		sets[*vmss.Name] = vmss
	}
	return sets
}

// getVirtualMachines makes a direct API call to fetch VMs without caching.
func (d *DirectResourceCache) getVirtualMachines() map[string][]compute.VirtualMachine {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	result, err := d.azClient.virtualMachinesClient.List(ctx, d.config.ResourceGroup)
	if err != nil {
		klog.Errorf("DirectCache: VirtualMachinesClient.List failed: %v", err)
		return make(map[string][]compute.VirtualMachine)
	}

	instances := make(map[string][]compute.VirtualMachine)
	for _, instance := range result {
		if instance.Tags == nil {
			continue
		}

		tags := instance.Tags
		vmPoolName := tags[agentpoolNameTag]
		if vmPoolName == nil {
			vmPoolName = tags[legacyAgentpoolNameTag]
		}
		if vmPoolName == nil {
			continue
		}

		poolName := *vmPoolName
		instances[poolName] = append(instances[poolName], instance)
	}
	return instances
}

// getVMsPoolMap makes a direct API call to fetch VMs pools without caching.
func (d *DirectResourceCache) getVMsPoolMap() map[string]armcontainerservice.AgentPool {
	if !d.config.EnableVMsAgentPool || d.azClient.agentPoolClient == nil {
		return make(map[string]armcontainerservice.AgentPool)
	}

	ctx, cancel := getContextWithTimeout(vmsContextTimeout)
	defer cancel()

	vmsPoolMap := make(map[string]armcontainerservice.AgentPool)
	pager := d.azClient.agentPoolClient.NewListPager(d.config.ClusterResourceGroup, d.config.ClusterName, nil)
	var aps []*armcontainerservice.AgentPool
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			klog.Errorf("DirectCache: agentPoolClient.pager.NextPage failed: %v", err)
			return make(map[string]armcontainerservice.AgentPool)
		}
		aps = append(aps, resp.Value...)
	}

	for _, ap := range aps {
		if ap != nil && ap.Name != nil && ap.Properties != nil && ap.Properties.Type != nil &&
			*ap.Properties.Type == armcontainerservice.AgentPoolTypeVirtualMachines {
			klog.V(6).Infof("DirectCache: Found VMs pool %q", *ap.Name)
			vmsPoolMap[*ap.Name] = *ap
		}
	}

	return vmsPoolMap
}

// getAutoscalingOptions returns autoscaling options for a scale set by direct lookup.
func (d *DirectResourceCache) getAutoscalingOptions(ref azureRef) map[string]string {
	scaleSets := d.getScaleSets()
	if vmss, ok := scaleSets[ref.Name]; ok {
		return extractAutoscalingOptionsFromScaleSetTags(vmss.Tags)
	}
	return make(map[string]string)
}

// FindForInstance finds the node group for a given instance by checking all registered groups.
func (d *DirectResourceCache) FindForInstance(instance *azureRef, vmType string) (cloudprovider.NodeGroup, error) {
	vmsPoolMap := d.getVMsPoolMap()
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	klog.V(4).Infof("DirectCache: FindForInstance: starts, ref: %s", instance.Name)
	resourceID, err := convertResourceGroupNameToLower(instance.Name)
	if err != nil {
		return nil, err
	}

	// Check virtual machines filtering logic (copied from azureCache)
	if vmType == providerazureconsts.VMTypeVMSS && len(vmsPoolMap) == 0 {
		scaleSets := d.getScaleSets()
		allUniform := true
		for _, scaleSet := range scaleSets {
			if scaleSet.VirtualMachineScaleSetProperties.OrchestrationMode == compute.Flexible {
				allUniform = false
				break
			}
		}
		if allUniform {
			if ok := virtualMachineRE.Match([]byte(resourceID)); ok {
				klog.V(3).Infof("DirectCache: Instance %q is not managed by vmss, omit it in autoscaler", instance.Name)
				return nil, nil
			}
		}
	}

	if vmType == providerazureconsts.VMTypeStandard {
		if ok := virtualMachineRE.Match([]byte(resourceID)); !ok {
			klog.V(3).Infof("DirectCache: Instance %q is not in Azure resource ID format, omit it in autoscaler", instance.Name)
			return nil, nil
		}
	}

	// Search through registered node groups by calling their Nodes() method directly
	for _, ng := range d.registeredNodeGroups {
		instances, err := ng.Nodes()
		if err != nil {
			continue
		}
		for _, inst := range instances {
			if strings.EqualFold(inst.Id, resourceID) {
				klog.V(4).Infof("DirectCache: found node group %q for instance", ng.Id())
				return ng, nil
			}
		}
	}

	klog.V(4).Infof("DirectCache: Couldn't find node group of instance %q", resourceID)
	return nil, nil
}

// HasInstance checks if an instance exists by direct API lookup.
func (d *DirectResourceCache) HasInstance(providerID string) (bool, error) {
	resourceID, err := convertResourceGroupNameToLower(providerID)
	if err != nil {
		return false, err
	}

	// Check in registered node groups by direct lookup
	d.mutex.RLock()
	defer d.mutex.RUnlock()

	for _, ng := range d.registeredNodeGroups {
		instances, err := ng.Nodes()
		if err != nil {
			continue
		}
		for _, inst := range instances {
			if strings.EqualFold(inst.Id, resourceID) {
				return true, nil
			}
		}
	}

	return false, cloudprovider.ErrNotImplemented
}

// HasVMSKUs always returns false since DirectCache doesn't use SKU caching.
func (d *DirectResourceCache) HasVMSKUs() bool {
	return false
}

// GetSKU creates a temporary SKU cache for single lookups.
func (d *DirectResourceCache) GetSKU(ctx context.Context, skuName, location string) (skewer.SKU, error) {
	if location == "" {
		return skewer.SKU{}, errors.New("location not specified")
	}

	// Create temporary cache for this lookup
	cache, err := skewer.NewCache(ctx,
		skewer.WithLocation(location),
		skewer.WithResourceClient(d.azClient.skuClient),
	)
	if err != nil {
		return skewer.SKU{}, err
	}

	return cache.Get(ctx, skuName, skewer.VirtualMachines, location)
}

// regenerate is a no-op for DirectCache since there's nothing to regenerate.
func (d *DirectResourceCache) regenerate() error {
	klog.V(4).Info("DirectCache: regenerate called - no-op in direct mode")
	return nil
}

// Cleanup is a no-op for DirectCache.
func (d *DirectResourceCache) Cleanup() {
	klog.V(4).Info("DirectCache: cleanup called - no-op in direct mode")
}