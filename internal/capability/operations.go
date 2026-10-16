package capability

// Operations a tool performs on its backends, named by capability rather than backend.
// A provider maps each to the permissions it needs (docs/design/0009).
const (
	OpWorkloadsGet     = "workloads.get"     // read one workload
	OpWorkloadsPods    = "workloads.pods"    // list a workload's pods and replica sets
	OpWorkloadsEvents  = "workloads.events"  // list events in a workload's namespace
	OpWorkloadsLogs    = "workloads.logs"    // read pod logs
	OpWorkloadsList    = "workloads.list"    // list workloads
	OpNamespacesList   = "namespaces.list"   // list a cluster's namespaces
	OpWorkloadsRestart = "workloads.restart" // restart a workload
	OpWorkloadsScale   = "workloads.scale"   // scale a workload, after checking autoscalers
	// OpApplicationsGet reads Argo CD Applications, with the delivery profile.
	OpApplicationsGet = "delivery.applications.get"
)
