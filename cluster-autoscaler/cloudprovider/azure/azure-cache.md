# Azure Cloud Provider Caching Architecture

The Azure cloud provider implements a sophisticated multi-layered caching system to optimize API calls and improve performance. Here's how it works:

## Core Cache Components

### 1. **azureCache** (`azure_cache.go:58-107`)
The main cache that tracks cluster resources:
- **Resource tracking**: Maps scale sets, VMs, and node pools
- **Instance-to-nodegroup mapping**: `instanceToNodeGroup` maps instances to their node groups
- **Refresh interval**: Configurable TTL (default 1 minute via `AZURE_VMSS_CACHE_TTL`)
- **Thread-safe**: Protected by `sync.Mutex`

### 2. **InstanceCache** (`azure_scale_set_instance_cache.go:55-71`)
Per-ScaleSet instance tracking:
- **Instance details**: Caches `cloudprovider.Instance` objects for each VMSS
- **Refresh period**: 5 minutes (configurable via `VmssVmsCacheTTL`)
- **Jitter support**: Prevents thundering herd with `instancesRefreshJitter`

### 3. **SKU Cache** (`azure_cache.go:126`)
VM SKU information cache:
- **Third-party library**: Uses `skewer.Cache` for Azure VM SKU data
- **Location-specific**: Filters SKUs by Azure region
- **Dynamic instance types**: Enables runtime SKU discovery

## Cache Hierarchies and Data Flow

### Main Cache Refresh Flow (`azure_manager.go:216-236`)
```
AzureManager.Refresh() → forceRefresh() → azureCache.regenerate()
├── fetchAzureResources() - Gets VMSS/VM lists from Azure API
├── Update instanceToNodeGroup mapping
└── Reset unownedInstances cache
```

### Instance Cache Refresh (`azure_scale_set_instance_cache.go:90-119`)
```
ScaleSet operations → validateInstanceCache() → updateInstanceCache()
├── Check lastInstanceRefresh + refreshPeriod vs current time
├── Call Azure VMSS VMs List API if stale
└── Update instanceCache with current VM status
```

### Resource Fetching Strategy (`azure_cache.go:233-260`)
1. **VMSS List**: `fetchScaleSets()` - Gets all scale sets in resource group
2. **VM List**: `fetchVirtualMachines()` - Gets standalone VMs grouped by agent pool
3. **VMs Pools**: `fetchVMsPools()` - Gets AKS VMs-type agent pools (if enabled)

## Cache Invalidation Patterns

### Automatic Invalidation
- **Time-based**: Each cache has its own TTL (1min azureCache, 5min instanceCache)
- **On registration changes**: `invalidateUnownedInstanceCache()` when node groups change
- **On scaling operations**: `invalidateInstanceCache()` forces immediate refresh

### Manual Invalidation (`azure_manager.go:238-243`)
- **Force refresh**: `invalidateCache()` sets `lastRefresh` to past time
- **Triggered by**: Node group registration/unregistration, scaling operations

## Cache Access Patterns

### Read Operations
- **GetNodeGroupForInstance**: `azureCache.FindForInstance()` uses `instanceToNodeGroup` map
- **ScaleSet.Nodes()**: Returns cached instances from `InstanceCache.instanceCache`
- **Instance lookups**: `getInstanceByProviderID()` searches instance cache

### Write Operations
- **Registration**: Updates `registeredNodeGroups` and rebuilds mappings
- **Status updates**: `setInstanceStatusByProviderID()` modifies cached instance status
- **Scaling decisions**: Updates both size tracking and instance caches

## Performance Optimizations

### Locking Strategy
- **Granular locks**: Separate mutexes for different cache components
- **Lock-free reads**: Some operations use lock-free patterns where safe
- **Minimize critical sections**: Fetch data outside locks, then update atomically

### Batch Operations
- **Bulk fetching**: Single API calls retrieve multiple resources
- **Parallel updates**: Multiple caches can refresh concurrently
- **Jittered refresh**: Prevents simultaneous cache refreshes across scale sets

### Memory Efficiency
- **Reference sharing**: Maps use references to avoid data duplication  
- **Selective caching**: Only caches actively managed resources
- **Cleanup**: `azureCache.Cleanup()` properly closes channels and releases resources

The caching system balances data freshness with API rate limiting, using configurable TTLs and intelligent invalidation to ensure the cluster autoscaler has current resource state while minimizing expensive Azure API calls.

# Azure Cloud Provider Caching Architecture Diagrams

## 1. Cache Component Hierarchy

```mermaid
graph TB
    AM[AzureManager] --> AC[azureCache]
    AM --> SS[ScaleSet 1..N]
    
    AC --> |stores| RG[registeredNodeGroups]
    AC --> |stores| ITN[instanceToNodeGroup map]
    AC --> |stores| UI[unownedInstances map]
    AC --> |stores| VMSS[scaleSets map]
    AC --> |stores| VM[virtualMachines map]
    AC --> |stores| VMP[vmsPoolMap]
    AC --> |stores| SKU[skus cache]
    
    SS --> |embedded| IC[InstanceCache]
    IC --> |stores| INST[instanceCache array]
    
    subgraph "Thread Safety"
        AC -.-> |sync.Mutex| MTX1[azureCache.mutex]
        IC -.-> |sync.Mutex| MTX2[instanceMutex]
        SS -.-> |sync.Mutex| MTX3[sizeMutex]
    end
```

## 2. Cache Refresh Flow

```mermaid
sequenceDiagram
    participant CAS as Cluster Autoscaler
    participant AM as AzureManager
    participant AC as azureCache
    participant Azure as Azure API
    
    Note over CAS,Azure: Every 10 seconds (autoscaler loop)
    CAS->>AM: Refresh()
    AM->>AM: Check lastRefresh + refreshInterval
    
    alt Cache is stale
        AM->>AM: forceRefresh()
        AM->>AC: fetchAutoNodeGroups()
        AM->>AC: regenerate()
        
        AC->>Azure: List VMSS
        AC->>Azure: List VMs
        AC->>Azure: List Agent Pools (if enabled)
        
        AC->>AC: Update scaleSets map
        AC->>AC: Update virtualMachines map
        AC->>AC: Update vmsPoolMap
        AC->>AC: Rebuild instanceToNodeGroup
        AC->>AC: Reset unownedInstances
        
        AM->>AM: Update lastRefresh
    else Cache is fresh
        AM-->>CAS: Return (no refresh needed)
    end
```

## 3. Instance Cache Lifecycle

```mermaid
stateDiagram-v2
    [*] --> Uninitialized
    
    Uninitialized --> Fresh: updateInstanceCache()
    Fresh --> Stale: Time > lastRefresh + refreshPeriod
    Stale --> Refreshing: validateInstanceCache()
    Refreshing --> Fresh: Azure API call complete
    
    Fresh --> Invalidated: invalidateInstanceCache()
    Invalidated --> Refreshing: Next access
    
    note right of Fresh
        instanceCache contains
        current VM instances
    end note
    
    note right of Refreshing
        Calls Azure VMSS VMs API
        Updates instanceCache array
    end note
```

## 4. Data Flow for Node Group Operations

```mermaid
flowchart TD
    subgraph "Node Group Lookup"
        A[GetNodeGroupForInstance] --> B[azureCache.FindForInstance]
        B --> C{Check unownedInstances}
        C -->|Found| D[Return nil]
        C -->|Not found| E[Search instanceToNodeGroup]
        E -->|Found| F[Return NodeGroup]
        E -->|Not found| G[Return nil + cache as unowned]
    end
    
    subgraph "Instance Operations"
        H[ScaleSet.Nodes] --> I[validateInstanceCache]
        I --> J{Cache valid?}
        J -->|Yes| K[Return cached instances]
        J -->|No| L[updateInstanceCache]
        L --> M[Call Azure VMSS VMs API]
        M --> N[Update instanceCache]
        N --> K
    end
    
    subgraph "Scaling Operations"
        O[Scale Up/Down] --> P[Update Azure VMSS]
        P --> Q[invalidateInstanceCache]
        Q --> R[Force refresh on next access]
    end
```

## 5. Cache Invalidation Triggers

```mermaid
mindmap
  root((Cache Invalidation))
    Time Based
      azureCache: 1 min default
      instanceCache: 5 min default
      SKU cache: Session based
    Operation Based
      Node Group Registration
        Register new group
        Unregister group
        Update min/max size
      Scaling Operations
        Scale up instances
        Scale down instances
        Delete instances
      Configuration Changes
        Auto-discovery changes
        Explicit config updates
    Manual Triggers
      forceRefresh()
      invalidateCache()
      invalidateInstanceCache()
```

## 6. Memory Layout and Relationships

```mermaid
erDiagram
    azureCache ||--o{ ScaleSet : "manages"
    azureCache ||--o{ AgentPool : "manages"
    azureCache ||--|| instanceToNodeGroup : "contains"
    azureCache ||--|| unownedInstances : "contains"
    azureCache ||--|| scaleSets : "contains"
    azureCache ||--|| virtualMachines : "contains"
    
    ScaleSet ||--|| InstanceCache : "embeds"
    InstanceCache ||--o{ Instance : "caches"
    
    instanceToNodeGroup }o--|| azureRef : "key"
    instanceToNodeGroup }o--|| NodeGroup : "value"
    
    unownedInstances }o--|| azureRef : "key"
    unownedInstances }o--|| bool : "value"
    
    scaleSets }o--|| string : "key (name)"
    scaleSets }o--|| VirtualMachineScaleSet : "value"
    
    virtualMachines }o--|| string : "key (pool name)"
    virtualMachines }o--o{ VirtualMachine : "value (array)"
```

## 7. Performance Optimization Strategies

```mermaid
graph LR
    subgraph "API Call Reduction"
        A[Bulk Fetching] --> A1[List all VMSS in one call]
        A[Bulk Fetching] --> A2[List all VMs in one call]
        B[Intelligent TTL] --> B1[1min for metadata]
        B[Intelligent TTL] --> B2[5min for instances]
        C[Jittered Refresh] --> C1[Prevent thundering herd]
    end
    
    subgraph "Memory Optimization"
        D[Reference Maps] --> D1[instanceToNodeGroup uses refs]
        E[Selective Caching] --> E1[Only cache managed resources]
        F[Cache Cleanup] --> F1[Proper resource cleanup]
    end
    
    subgraph "Concurrency Control"
        G[Granular Locking] --> G1[Separate mutexes per cache]
        H[Lock Minimization] --> H1[Fetch outside critical sections]
        I[Lock-free Reads] --> I1[Where data races are safe]
    end
```

## 8. Cache Consistency Patterns

```mermaid
sequenceDiagram
    participant App as Application
    participant Cache as Cache Layer
    participant Azure as Azure API
    
    Note over App,Azure: Read-Through Pattern
    App->>Cache: Get Instance Data
    Cache->>Cache: Check TTL
    alt Data is stale
        Cache->>Azure: Fetch fresh data
        Azure-->>Cache: Return data
        Cache->>Cache: Update cache
    end
    Cache-->>App: Return data
    
    Note over App,Azure: Write-Through Pattern
    App->>Cache: Scale Operation
    Cache->>Azure: Update VMSS
    Azure-->>Cache: Confirm update
    Cache->>Cache: Invalidate affected caches
    Cache-->>App: Operation complete
```

These diagrams illustrate how the Azure cloud provider implements a sophisticated multi-layered caching system that balances performance with data consistency, using time-based invalidation, intelligent refresh patterns, and careful concurrency control to minimize Azure API calls while maintaining accurate cluster state.