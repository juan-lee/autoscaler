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
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/skewer"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"context"
)

// ResourceCache defines the interface for Azure resource caching operations.
// Implementations can provide either cached or direct API access modes.
type ResourceCache interface {
	// Node group management
	Register(nodeGroup cloudprovider.NodeGroup) bool
	Unregister(nodeGroup cloudprovider.NodeGroup) bool
	getRegisteredNodeGroups() []cloudprovider.NodeGroup

	// Instance lookups
	FindForInstance(instance *azureRef, vmType string) (cloudprovider.NodeGroup, error)
	HasInstance(providerID string) (bool, error)

	// Resource access
	getScaleSets() map[string]compute.VirtualMachineScaleSet
	getVirtualMachines() map[string][]compute.VirtualMachine
	getVMsPoolMap() map[string]armcontainerservice.AgentPool
	getAutoscalingOptions(ref azureRef) map[string]string

	// SKU operations
	HasVMSKUs() bool
	GetSKU(ctx context.Context, skuName, location string) (skewer.SKU, error)

	// Cache management
	regenerate() error
	Cleanup()
}