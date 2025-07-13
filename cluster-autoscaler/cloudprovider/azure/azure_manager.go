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

//go:generate go run azure_instance_types/gen.go

package azure

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/skewer"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	klog "k8s.io/klog/v2"
	providerazureconsts "sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

const (
	azurePrefix = "azure://"

	scaleToZeroSupportedStandard = false
	scaleToZeroSupportedVMSS     = true
	refreshInterval              = 1 * time.Minute
)

// AzureManager handles Azure communication and data caching.
type AzureManager struct {
	config   *Config
	azClient *azClient
	env      azure.Environment

	// registeredNodeGroups tracks all known NodeGroups without caching
	registeredNodeGroups []cloudprovider.NodeGroup
	// nodeGroupsLock protects access to registeredNodeGroups
	nodeGroupsLock sync.RWMutex

	// skuCache for dynamic instance list functionality  
	skuCache *skewer.Cache
	skuCacheLock sync.RWMutex

	autoDiscoverySpecs   []labelAutoDiscoveryConfig
	explicitlyConfigured map[string]bool
}

// createAzureManagerInternal allows for a custom azClient to be passed in by tests.
func createAzureManagerInternal(configReader io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions, azClient *azClient) (*AzureManager, error) {
	cfg, err := BuildAzureConfig(configReader)
	if err != nil {
		return nil, err
	}

	// Defaulting env to Azure Public Cloud.
	env := azure.PublicCloud
	if cfg.Cloud != "" {
		env, err = azure.EnvironmentFromName(cfg.Cloud)
		if err != nil {
			return nil, err
		}
	}

	klog.Infof("Starting azure manager with subscription ID %q", cfg.SubscriptionID)

	if azClient == nil {
		azClient, err = newAzClient(cfg, &env)
		if err != nil {
			return nil, err
		}
	}

	// Create azure manager.
	manager := &AzureManager{
		config:               cfg,
		env:                  env,
		azClient:             azClient,
		registeredNodeGroups: make([]cloudprovider.NodeGroup, 0),
		skuCache:             &skewer.Cache{},
		explicitlyConfigured: make(map[string]bool),
	}

	// Initialize SKU cache if dynamic instance list is enabled
	if cfg.EnableDynamicInstanceList {
		if err := manager.initializeSKUCache(cfg.Location); err != nil {
			klog.Errorf("Error while populating SKU list: %v", err)
			cfg.EnableDynamicInstanceList = false
			klog.Warning("No VM SKU info loaded, using only static SKU list")
		} else {
			klog.V(2).Infof("Successfully initialized SKU cache for dynamic instance list")
		}
	}

	specs, err := ParseLabelAutoDiscoverySpecs(discoveryOpts)
	if err != nil {
		return nil, err
	}
	manager.autoDiscoverySpecs = specs

	if err := manager.fetchExplicitNodeGroups(discoveryOpts.NodeGroupSpecs); err != nil {
		return nil, err
	}


	return manager, nil
}

// CreateAzureManager creates Azure Manager object to work with Azure.
func CreateAzureManager(configReader io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions) (*AzureManager, error) {
	return createAzureManagerInternal(configReader, discoveryOpts, nil)
}

func (m *AzureManager) fetchExplicitNodeGroups(specs []string) error {
	for _, spec := range specs {
		nodeGroup, err := m.buildNodeGroupFromSpec(spec)
		if err != nil {
			return fmt.Errorf("failed to parse node group spec: %v", err)
		}
		m.RegisterNodeGroup(nodeGroup)
		m.explicitlyConfigured[nodeGroup.Id()] = true
	}

	return nil
}

func (m *AzureManager) buildNodeGroupFromSpec(spec string) (cloudprovider.NodeGroup, error) {
	scaleToZeroSupported := scaleToZeroSupportedStandard
	if strings.EqualFold(m.config.VMType, providerazureconsts.VMTypeVMSS) {
		scaleToZeroSupported = scaleToZeroSupportedVMSS
	}
	s, err := dynamic.SpecFromString(spec, scaleToZeroSupported)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node group spec: %v", err)
	}
	// Check if this is a VMS pool by examining Azure VMs directly
	if isVMsPool, err := m.isVMsPool(s.Name); err != nil {
		klog.Warningf("Failed to check if %s is VMS pool: %v", s.Name, err)
	} else if isVMsPool {
		return NewVMsPool(s, m), nil
	}

	switch m.config.VMType {
	case providerazureconsts.VMTypeStandard:
		return NewAgentPool(s, m)
	case providerazureconsts.VMTypeVMSS:
		return NewScaleSet(s, m, false)
	default:
		return nil, fmt.Errorf("vmtype %s not supported", m.config.VMType)
	}
}

// isVMsPool checks if the given nodepool name corresponds to a VMS pool by examining Azure VMs
func (m *AzureManager) isVMsPool(nodepoolName string) (bool, error) {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	result, err := m.azClient.virtualMachinesClient.List(ctx, m.config.ResourceGroup)
	if err != nil {
		return false, err.Error()
	}

	const (
		legacyAgentpoolNameTag = "poolName"
		agentpoolNameTag       = "aks-managed-poolName"
		agentpoolTypeTag       = "aks-managed-agentpool-type"
		vmsPoolType            = "VirtualMachines"
	)

	for _, vm := range result {
		if vm.Tags == nil {
			continue
		}

		tags := vm.Tags
		vmPoolName := tags[agentpoolNameTag]
		// fall back to legacy tag name if not found
		if vmPoolName == nil {
			vmPoolName = tags[legacyAgentpoolNameTag]
		}
		if vmPoolName == nil || *vmPoolName != nodepoolName {
			continue
		}

		// nodes from vms pool will have tag "aks-managed-agentpool-type" set to "VirtualMachines"
		if agentpoolType := tags[agentpoolTypeTag]; agentpoolType != nil {
			return strings.EqualFold(*agentpoolType, vmsPoolType), nil
		}
	}
	return false, nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state.
// In particular the list of node groups returned by NodeGroups can change as a result of CloudProvider.Refresh().
func (m *AzureManager) Refresh() error {
	if err := m.fetchAutoNodeGroups(); err != nil {
		klog.Errorf("Failed to fetch autodiscovered nodegroups: %v", err)
		return err
	}
	return nil
}


// Fetch automatically discovered NodeGroups. These NodeGroups should be unregistered if
// they no longer exist in Azure.
func (m *AzureManager) fetchAutoNodeGroups() error {
	groups, err := m.getFilteredNodeGroups(m.autoDiscoverySpecs)
	if err != nil {
		return fmt.Errorf("cannot autodiscover NodeGroups: %s", err)
	}

	exists := make(map[string]bool)
	for _, group := range groups {
		id := group.Id()
		exists[id] = true
		if m.explicitlyConfigured[id] {
			// This NodeGroup was explicitly configured, but would also be
			// autodiscovered. We want the explicitly configured min and max
			// nodes to take precedence.
			klog.V(3).Infof("Ignoring explicitly configured NodeGroup %s for autodiscovery.", group.Id())
			continue
		}
		if m.RegisterNodeGroup(group) {
			klog.V(3).Infof("Autodiscovered NodeGroup %s using tags %v", group.Id(), m.autoDiscoverySpecs)
		}
	}

	for _, nodeGroup := range m.getNodeGroups() {
		nodeGroupID := nodeGroup.Id()
		if !exists[nodeGroupID] && !m.explicitlyConfigured[nodeGroupID] {
			m.UnregisterNodeGroup(nodeGroup)
		}
	}

	return nil
}

func (m *AzureManager) getNodeGroups() []cloudprovider.NodeGroup {
	m.nodeGroupsLock.RLock()
	defer m.nodeGroupsLock.RUnlock()
	return m.registeredNodeGroups
}

// RegisterNodeGroup registers a NodeGroup.
func (m *AzureManager) RegisterNodeGroup(nodeGroup cloudprovider.NodeGroup) bool {
	m.nodeGroupsLock.Lock()
	defer m.nodeGroupsLock.Unlock()

	for i := range m.registeredNodeGroups {
		if existing := m.registeredNodeGroups[i]; strings.EqualFold(existing.Id(), nodeGroup.Id()) {
			if existing.MinSize() == nodeGroup.MinSize() && existing.MaxSize() == nodeGroup.MaxSize() {
				// Node group is already registered and min/max size haven't changed, no action required.
				return false
			}
			m.registeredNodeGroups[i] = nodeGroup
			klog.V(4).Infof("Node group %q updated", nodeGroup.Id())
			return true
		}
	}

	klog.V(4).Infof("Registering Node Group %q", nodeGroup.Id())
	m.registeredNodeGroups = append(m.registeredNodeGroups, nodeGroup)
	return true
}

// UnregisterNodeGroup unregisters a NodeGroup.
func (m *AzureManager) UnregisterNodeGroup(nodeGroup cloudprovider.NodeGroup) bool {
	m.nodeGroupsLock.Lock()
	defer m.nodeGroupsLock.Unlock()

	updated := make([]cloudprovider.NodeGroup, 0, len(m.registeredNodeGroups))
	changed := false
	for _, existing := range m.registeredNodeGroups {
		if strings.EqualFold(existing.Id(), nodeGroup.Id()) {
			klog.V(1).Infof("Unregistered node group %s", nodeGroup.Id())
			changed = true
			continue
		}
		updated = append(updated, existing)
	}
	m.registeredNodeGroups = updated
	return changed
}

// GetNodeGroupForInstance returns the NodeGroup of the given Instance
func (m *AzureManager) GetNodeGroupForInstance(instance *azureRef) (cloudprovider.NodeGroup, error) {
	m.nodeGroupsLock.RLock()
	defer m.nodeGroupsLock.RUnlock()

	// Check each registered node group to see if the instance belongs to it
	for _, nodeGroup := range m.registeredNodeGroups {
		instances, err := nodeGroup.Nodes()
		if err != nil {
			klog.Warningf("Failed to get nodes for nodegroup %s: %v", nodeGroup.Id(), err)
			continue
		}
		
		for _, inst := range instances {
			if strings.EqualFold(inst.Id, instance.Name) {
				return nodeGroup, nil
			}
		}
	}
	
	return nil, nil
}

// HasInstance checks if the given providerID exists by checking all registered node groups
func (m *AzureManager) HasInstance(providerID string) (bool, error) {
	resourceID, err := convertResourceGroupNameToLower(providerID)
	if err != nil {
		// Most likely an invalid resource id, we should return an error
		return false, err
	}

	instanceRef := &azureRef{Name: resourceID}
	nodeGroup, err := m.GetNodeGroupForInstance(instanceRef)
	if err != nil {
		return false, err
	}
	
	// If we found a node group, the instance exists
	return nodeGroup != nil, nil
}

// getAutoscalingOptionsFromVMSS retrieves autoscaling options from VMSS tags
func (m *AzureManager) getAutoscalingOptionsFromVMSS(scaleSetName string) map[string]string {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	vmss, rerr := m.azClient.virtualMachineScaleSetsClient.Get(ctx, m.config.ResourceGroup, scaleSetName)
	if rerr != nil {
		klog.Warningf("Failed to get VMSS %s: %v", scaleSetName, rerr)
		return nil
	}

	return extractAutoscalingOptionsFromScaleSetTags(vmss.Tags)
}

// GetScaleSetOptions parse options extracted from VMSS tags and merges them with provided defaults
func (m *AzureManager) GetScaleSetOptions(scaleSetName string, defaults config.NodeGroupAutoscalingOptions) *config.NodeGroupAutoscalingOptions {
	options := m.getAutoscalingOptionsFromVMSS(scaleSetName)
	if options == nil || len(options) == 0 {
		return &defaults
	}

	if opt, ok := getFloat64Option(options, scaleSetName, config.DefaultScaleDownUtilizationThresholdKey); ok {
		defaults.ScaleDownUtilizationThreshold = opt
	}
	if opt, ok := getFloat64Option(options, scaleSetName, config.DefaultScaleDownGpuUtilizationThresholdKey); ok {
		defaults.ScaleDownGpuUtilizationThreshold = opt
	}
	if opt, ok := getDurationOption(options, scaleSetName, config.DefaultScaleDownUnneededTimeKey); ok {
		defaults.ScaleDownUnneededTime = opt
	}
	if opt, ok := getDurationOption(options, scaleSetName, config.DefaultScaleDownUnreadyTimeKey); ok {
		defaults.ScaleDownUnreadyTime = opt
	}

	return &defaults
}

// Cleanup performs any necessary cleanup operations.
func (m *AzureManager) Cleanup() {
	// No cleanup needed since we're not using caching
}

func (m *AzureManager) getFilteredNodeGroups(filter []labelAutoDiscoveryConfig) (nodeGroups []cloudprovider.NodeGroup, err error) {
	if len(filter) == 0 {
		return nil, nil
	}

	if m.config.VMType == providerazureconsts.VMTypeVMSS {
		return m.getFilteredScaleSets(filter)
	}

	return nil, fmt.Errorf("vmType %q does not support autodiscovery", m.config.VMType)
}

// getScaleSets returns all scale sets in the resource group using Azure API
func (m *AzureManager) getScaleSets() (map[string]compute.VirtualMachineScaleSet, error) {
	ctx, cancel := getContextWithTimeout(vmssContextTimeout)
	defer cancel()

	result, err := m.azClient.virtualMachineScaleSetsClient.List(ctx, m.config.ResourceGroup)
	if err != nil {
		klog.Errorf("VirtualMachineScaleSetsClient.List in resource group %q failed: %v", m.config.ResourceGroup, err)
		return nil, err.Error()
	}

	sets := make(map[string]compute.VirtualMachineScaleSet)
	for _, vmss := range result {
		sets[*vmss.Name] = vmss
	}
	return sets, nil
}

// getVirtualMachines returns virtual machines grouped by pool name using Azure API
func (m *AzureManager) getVirtualMachines() (map[string][]compute.VirtualMachine, error) {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	result, err := m.azClient.virtualMachinesClient.List(ctx, m.config.ResourceGroup)
	if err != nil {
		klog.Errorf("VirtualMachinesClient.List in resource group %q failed: %v", m.config.ResourceGroup, err)
		return nil, err.Error()
	}

	const (
		legacyAgentpoolNameTag = "poolName"
		agentpoolNameTag       = "aks-managed-poolName"
	)

	instances := make(map[string][]compute.VirtualMachine)
	for _, instance := range result {
		if instance.Tags == nil {
			continue
		}

		tags := instance.Tags
		vmPoolName := tags[agentpoolNameTag]
		// fall back to legacy tag name if not found
		if vmPoolName == nil {
			vmPoolName = tags[legacyAgentpoolNameTag]
		}
		if vmPoolName == nil {
			continue
		}

		instances[*vmPoolName] = append(instances[*vmPoolName], instance)
	}
	return instances, nil
}

// initializeSKUCache initializes the SKU cache for dynamic instance list functionality
func (m *AzureManager) initializeSKUCache(location string) error {
	if location == "" {
		return fmt.Errorf("location not specified")
	}

	cache, err := skewer.NewCache(context.Background(),
		skewer.WithLocation(location),
		skewer.WithResourceClient(m.azClient.skuClient),
	)
	if err != nil {
		return err
	}

	m.skuCacheLock.Lock()
	defer m.skuCacheLock.Unlock()
	m.skuCache = cache
	return nil
}

// HasVMSKUs returns true if the manager has any VM SKUs loaded
func (m *AzureManager) HasVMSKUs() bool {
	m.skuCacheLock.RLock()
	defer m.skuCacheLock.RUnlock()
	return !(m.skuCache == nil || m.skuCache.Equal(&skewer.Cache{}))
}

// GetSKU retrieves SKU information for the given SKU name and location
func (m *AzureManager) GetSKU(ctx context.Context, skuName, location string) (skewer.SKU, error) {
	m.skuCacheLock.RLock()
	defer m.skuCacheLock.RUnlock()
	return m.skuCache.Get(ctx, skuName, skewer.VirtualMachines, location)
}

// getFilteredScaleSets gets a list of scale sets and instanceIDs.
func (m *AzureManager) getFilteredScaleSets(filter []labelAutoDiscoveryConfig) ([]cloudprovider.NodeGroup, error) {
	vmssList, err := m.getScaleSets()
	if err != nil {
		return nil, err
	}

	var nodeGroups []cloudprovider.NodeGroup
	for _, scaleSet := range vmssList {
		var cfgSizes *autoDiscoveryConfigSizes
		if len(filter) > 0 {
			if scaleSet.Tags == nil || len(scaleSet.Tags) == 0 {
				continue
			}

			if cfgSizes = matchDiscoveryConfig(scaleSet.Tags, filter); cfgSizes == nil {
				continue
			}
		}
		spec := &dynamic.NodeGroupSpec{
			Name:               *scaleSet.Name,
			MinSize:            1,
			MaxSize:            -1,
			SupportScaleToZero: scaleToZeroSupportedVMSS,
		}

		if val, ok := scaleSet.Tags["min"]; ok {
			if minSize, err := strconv.Atoi(*val); err == nil {
				spec.MinSize = minSize
			} else {
				klog.Warningf("ignoring vmss %q because of invalid minimum size specified for vmss: %s", *scaleSet.Name, err)
				continue
			}
		} else if cfgSizes.Min >= 0 {
			spec.MinSize = cfgSizes.Min
		} else {
			klog.Warningf("ignoring vmss %q because of no minimum size specified for vmss", *scaleSet.Name)
			continue
		}
		if spec.MinSize < 0 {
			klog.Warningf("ignoring vmss %q because of minimum size must be a non-negative number of nodes", *scaleSet.Name)
			continue
		}
		if val, ok := scaleSet.Tags["max"]; ok {
			if maxSize, err := strconv.Atoi(*val); err == nil {
				spec.MaxSize = maxSize
			} else {
				klog.Warningf("ignoring vmss %q because of invalid maximum size specified for vmss: %s", *scaleSet.Name, err)
				continue
			}
		} else if cfgSizes.Max >= 0 {
			spec.MaxSize = cfgSizes.Max
		} else {
			klog.Warningf("ignoring vmss %q because of no maximum size specified for vmss", *scaleSet.Name)
			continue
		}
		if spec.MaxSize < spec.MinSize {
			klog.Warningf("ignoring vmss %q because of maximum size must be greater than or equal to minimum size: max=%d < min=%d", *scaleSet.Name, spec.MaxSize, spec.MinSize)
			continue
		}

		dedicatedHost := scaleSet.VirtualMachineScaleSetProperties != nil && scaleSet.VirtualMachineScaleSetProperties.HostGroup != nil

		vmss, err := NewScaleSet(spec, m, dedicatedHost)
		if err != nil {
			klog.Warningf("ignoring vmss %q %s", *scaleSet.Name, err)
			continue
		}
		nodeGroups = append(nodeGroups, vmss)
	}

	return nodeGroups, nil
}
