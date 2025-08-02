# Kubernetes Cluster Autoscaler Main Control Loop

This diagram shows the main control loop of the Kubernetes Cluster Autoscaler, illustrating how it makes scaling decisions based on cluster state and workload demands.

```mermaid
flowchart TD
    A[Main Loop Start] --> B[Update Cluster State]
    B --> C[Get Unschedulable Pods]
    C --> D[Process Pod List]
    D --> E{Are there unschedulable pods?}
    
    E -->|Yes| F[Scale Up Decision]
    E -->|No| G[Scale Down Decision]
    
    F --> H[Estimate Required Nodes]
    H --> I[Select Node Groups via Expander]
    I --> J[Simulate Pod Placement]
    J --> K{Simulation Valid?}
    K -->|Yes| L[Trigger Node Creation]
    K -->|No| M[Log Scale Up Failure]
    
    G --> N[Find Underutilized Nodes]
    N --> O[Check Scale Down Conditions]
    O --> P{Can scale down safely?}
    P -->|Yes| Q[Select Nodes for Removal]
    P -->|No| R[Skip Scale Down]
    
    Q --> S[Simulate Pod Rescheduling]
    S --> T{Rescheduling Safe?}
    T -->|Yes| U[Drain and Delete Nodes]
    T -->|No| V[Mark Nodes as Unremovable]
    
    L --> W[Update Metrics]
    M --> W
    R --> W
    U --> W
    V --> W
    
    W --> X[Process Status Updates]
    X --> Y[Handle Events]
    Y --> Z[Sleep Until Next Iteration]
    Z --> A
    
    %% Parallel processes
    B -.-> B1[Node Info Provider]
    B -.-> B2[Custom Resources Processor]
    B -.-> B3[Node Group Set Processor]
    
    %% Decision factors
    E -.-> E1[Pod Age Check]
    E -.-> E2[GPU Pod Buffer Time]
    E -.-> E3[Resource Requirements]
    
    P -.-> P1[Pod Disruption Budget]
    P -.-> P2[Node Utilization]
    P -.-> P3[Scale Down Delays]
    P -.-> P4[Daemonset Pods]
```

## Key Components

### Scale Up Path
- **Estimate Required Nodes**: Uses bin-packing algorithms to determine resource needs
- **Select Node Groups**: Applies expander strategies (random, priority, cost-based, etc.)
- **Simulate Pod Placement**: Validates that pods can actually be scheduled on new nodes
- **Trigger Node Creation**: Calls cloud provider APIs to create new nodes

### Scale Down Path
- **Find Underutilized Nodes**: Identifies nodes with low resource utilization
- **Check Scale Down Conditions**: Validates safety constraints and delays
- **Simulate Pod Rescheduling**: Ensures pods can be moved to other nodes
- **Drain and Delete Nodes**: Safely removes nodes after moving workloads

### Decision Points
- **Unschedulable Pods Check**: Determines whether scale up is needed
- **Simulation Validation**: Ensures scaling actions are safe and effective
- **Safety Checks**: Validates Pod Disruption Budgets, node utilization, and other constraints

### Processing Pipeline
- **Update Cluster State**: Refreshes node and pod information
- **Process Pod List**: Filters and categorizes pods
- **Update Metrics**: Publishes monitoring data
- **Handle Events**: Processes Kubernetes events and status updates

The loop runs continuously, making incremental scaling decisions to maintain optimal cluster capacity while ensuring workload availability and cost efficiency.


