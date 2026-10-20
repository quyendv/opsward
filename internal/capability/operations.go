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

// WorkloadsOps maps each Workloads method to the operations it performs. Tests hold the
// tools' declarations and the Kubernetes provider's rules to it.
var WorkloadsOps = map[string][]string{
	"GetWorkload":      {OpWorkloadsGet, OpWorkloadsPods},
	"WorkloadLabels":   {OpWorkloadsGet},
	"WorkloadMetadata": {OpWorkloadsGet},
	"WorkloadEvents":   {OpWorkloadsGet, OpWorkloadsPods, OpWorkloadsEvents},
	"WorkloadLogs":     {OpWorkloadsGet, OpWorkloadsPods, OpWorkloadsLogs},
	"PodNamePattern":   {OpWorkloadsGet, OpWorkloadsPods},
	"ListNamespaces":   {OpNamespacesList},
	"ListWorkloads":    {OpWorkloadsList},
	"WorkloadRestarts": {OpWorkloadsPods},
	"RestartWorkload":  {OpWorkloadsGet, OpWorkloadsRestart},
	"ScaleWorkload":    {OpWorkloadsGet, OpWorkloadsScale},
}
