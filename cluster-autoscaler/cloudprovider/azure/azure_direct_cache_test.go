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
	"testing"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/framework"
)

func TestDirectResourceCache(t *testing.T) {
	config := &Config{
		DisableCaching: true,
	}

	// Create direct cache
	cache := newDirectResourceCache(nil, config)

	// Test basic operations
	assert.NotNil(t, cache)
	assert.Empty(t, cache.getRegisteredNodeGroups())
	assert.False(t, cache.HasVMSKUs())

	// Test cleanup (should be no-op)
	cache.Cleanup()

	// Test regenerate (should be no-op)
	err := cache.regenerate()
	assert.NoError(t, err)
}

func TestNewResourceCacheFactory(t *testing.T) {
	// Test direct mode (doesn't require azClient)
	directConfig := &Config{
		DisableCaching: true,
	}

	directCache, err := newResourceCache(nil, directConfig)
	assert.NoError(t, err)
	assert.NotNil(t, directCache)
	// Should be DirectResourceCache type
	_, ok := directCache.(*DirectResourceCache)
	assert.True(t, ok, "Expected DirectResourceCache for direct mode")

	// Note: We skip testing azureCache creation with nil azClient
	// since it would require a full mock setup, which is covered in other tests
}

func TestDirectCacheNodeGroupManagement(t *testing.T) {
	config := &Config{
		DisableCaching: true,
	}

	cache := newDirectResourceCache(nil, config)

	// Create mock node group
	mockNG := &mockNodeGroup{id: "test-ng"}

	// Test registration
	registered := cache.Register(mockNG)
	assert.True(t, registered)

	nodeGroups := cache.getRegisteredNodeGroups()
	assert.Len(t, nodeGroups, 1)
	assert.Equal(t, "test-ng", nodeGroups[0].Id())

	// Test registration of same group (should not add duplicate)
	registered = cache.Register(mockNG)
	assert.False(t, registered)

	nodeGroups = cache.getRegisteredNodeGroups()
	assert.Len(t, nodeGroups, 1)

	// Test unregistration
	unregistered := cache.Unregister(mockNG)
	assert.True(t, unregistered)

	nodeGroups = cache.getRegisteredNodeGroups()
	assert.Len(t, nodeGroups, 0)
}

// Mock node group for testing
type mockNodeGroup struct {
	id string
}

func (m *mockNodeGroup) MaxSize() int                                   { return 10 }
func (m *mockNodeGroup) MinSize() int                                   { return 1 }
func (m *mockNodeGroup) TargetSize() (int, error)                       { return 3, nil }
func (m *mockNodeGroup) IncreaseSize(delta int) error                   { return nil }
func (m *mockNodeGroup) DecreaseTargetSize(delta int) error             { return nil }
func (m *mockNodeGroup) DeleteNodes(nodes []*apiv1.Node) error          { return nil }
func (m *mockNodeGroup) Id() string                                     { return m.id }
func (m *mockNodeGroup) Debug() string                                  { return m.id }
func (m *mockNodeGroup) Nodes() ([]cloudprovider.Instance, error)       { return nil, nil }
func (m *mockNodeGroup) TemplateNodeInfo() (*framework.NodeInfo, error) { return nil, nil }
func (m *mockNodeGroup) Exist() bool                                    { return true }
func (m *mockNodeGroup) Create() (cloudprovider.NodeGroup, error)       { return m, nil }
func (m *mockNodeGroup) Delete() error                                  { return nil }
func (m *mockNodeGroup) Autoprovisioned() bool                          { return false }
func (m *mockNodeGroup) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	return &defaults, nil
}
func (m *mockNodeGroup) AtomicIncreaseSize(delta int) error         { return nil }
func (m *mockNodeGroup) ForceDeleteNodes(nodes []*apiv1.Node) error { return nil }
