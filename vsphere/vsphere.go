/*
http://www.apache.org/licenses/LICENSE-2.0.txt


Copyright 2017 Intel Corporation

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

package vsphere

import (
	"context"
	"fmt"

	"github.com/intelsdi-x/snap-plugin-lib-go/v1/plugin"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	vendor = "intel"
	class  = "vmware"
	name   = "vsphere"

	aggregatedNs = "aggr"

	nsSource = 3 // Source entity - host, datastore etc.

	nsHost         = 4
	nsHostGroup    = 5
	nsHostInstance = 6
	nsHostMetric   = 7

	nsVM         = 6
	nsVMGroup    = 7
	nsVMInstance = 8
	nsVMMetric   = 9

	nsClusterGroup    = 3
	nsCluster         = 4
	nsClusterInstance = 5
	nsClusterMetric   = 6

	unitKilobyte = 1024
	unitMegabyte = unitKilobyte * 1024
)

/*
Collector implements plugin interface
GovomiResources contains methods to retrieve data from vSphereAPI
*/
type Collector struct {
	GovmomiResources *govmomiClient
}

type parsedQueryResponse struct {
	hostName        string
	vmName          string
	counterFullName string
	instance        string
	data            int64
}

// perfQuerySpecMap holds map of [entity name]types.PerfQuerySpec
type perfQuerySpecMap map[string]types.PerfQuerySpec

// Metric dependency map
// Maps Snap metrics to vSphere perf counters needed to calculate desired metric
// Each Snap metric can contain multiple dependencies
var metricDepMap = map[string]map[string][]string{
	"cpu": map[string][]string{
		"idle": []string{"cpu.usage.average"},
		"wait": []string{"cpu.latency.average"},
		"load": []string{"rescpu.actav1.latest"},
	},
	"mem": map[string][]string{
		"usage":     []string{"mem.consumed.average"},
		"free":      []string{"mem.consumed.average"},
		"swapUsage": []string{"mem.swapused.average"},
	},
	"net": map[string][]string{
		"kbrateTx":  []string{"net.bytesTx.average"},
		"kbrateRx":  []string{"net.bytesRx.average"},
		"packetsTx": []string{"net.packetsTx.summation"},
		"packetsRx": []string{"net.packetsRx.summation"},
	},
	"virtualDisk": map[string][]string{
		"readIops":        []string{"virtualDisk.numberReadAveraged.average"},
		"writeIops":       []string{"virtualDisk.numberWriteAveraged.average"},
		"readThroughput":  []string{"virtualDisk.read.average"},
		"writeThroughput": []string{"virtualDisk.write.average"},
		"readLatency":     []string{"virtualDisk.totalReadLatency.average"},
		"writeLatency":    []string{"virtualDisk.totalWriteLatency.average"},
	},
	"cluster": map[string][]string{
		"cpuCapacity":    []string{"cpu.totalmhz.average"},
		"cpuUsage":       []string{"clusterServices.effectivecpu.average"},
		"memCapacity":    []string{"mem.totalmb.average"},
		"memUsage":       []string{"clusterServices.effectivemem.average"},
		"datastoreRead":  []string{"datastore.read.average"},
		"datastoreWrite": []string{"datastore.write.average"},
		"netPacketsTx":   []string{"net.throughput.pktsTx.average"},
		"netPacketsRx":   []string{"net.throughput.pktsRx.average"},
	},
}

// New returns instance of VsphereCollector
func New(isTest bool) *Collector {
	collector := &Collector{}
	collector.GovmomiResources = &govmomiClient{}
	if isTest {
		//collector.GovmomiResources.api = &mockAPI{}
	} else {
		collector.GovmomiResources.api = &govmomiAPI{}
	}

	return collector
}

// preparePerfMetricID creates types.PerfMetricId object for given counter name
func (c *Collector) preparePerfMetricID(ctx context.Context, fullCounterName string) (*types.PerfMetricId, error) {
	ctr, err := c.GovmomiResources.FindCounter(ctx, fullCounterName)
	if err != nil {
		return nil, err
	}
	metric := &types.PerfMetricId{
		Instance:  defaultMetricInstance, // Get data for all instances (i.e. all cores)
		CounterId: ctr.Key,
	}
	return metric, nil
}

// updateQuerySpecMap updates query spec map with new PerfMetricIds (based on given metric name and entity reference)
func (c *Collector) updateQuerySpecMap(ctx context.Context, querySpecs perfQuerySpecMap, interval int32, group string, metric string, entityName string, entityRef types.ManagedObjectReference) error {
	// Initialize query spec map entry if needed
	if _, ok := querySpecs[entityName]; !ok {
		querySpecs[entityName] = types.PerfQuerySpec{
			Entity:     entityRef,
			IntervalId: interval,
			MaxSample:  1,
			Format:     "normal",
			MetricId:   []types.PerfMetricId{},
		}
	}

	counterFullNames := metricDepMap[group][metric]
	if counterFullNames != nil {
		for _, ctr := range counterFullNames {
			// Prepare types.PerfMetricId object based on counter name
			ctrMetricID, err := c.preparePerfMetricID(ctx, ctr)
			if err != nil {
				return err
			}

			// Add prepared metric to query spec map (for selected entity), avoid duplicates
			entitySpec := querySpecs[entityName]
			duplicate := false
			for _, mID := range entitySpec.MetricId {
				if mID.CounterId == ctrMetricID.CounterId {
					duplicate = true
				}
			}
			if !duplicate {
				entitySpec.MetricId = append(entitySpec.MetricId, *ctrMetricID)
				querySpecs[entityName] = entitySpec
			}
		}
	}

	return nil
}

// buildQuerySpecsForMetrics builds slice of perf counter queries for all counters, hosts and virtual machines provided in metric namespaces
func (c *Collector) buildQuerySpecsForMetrics(ctx context.Context, mts []plugin.Metric) ([]types.PerfQuerySpec, error) {
	hostQuerySpecs := make(perfQuerySpecMap)
	vmQuerySpecs := make(perfQuerySpecMap)
	clusterQuerySpecs := make(perfQuerySpecMap)
	allQuerySpecs := []types.PerfQuerySpec{}

	c.GovmomiResources.ClearCache()

	for _, m := range mts {
		if m.Namespace[nsSource].Value == "host" {
			// Retrieve hosts with name given in namespace entry
			hosts, err := c.GovmomiResources.FindHosts(ctx, m.Namespace[nsHost].Value)
			if err != nil {
				return nil, err
			}

			for _, host := range hosts {
				isHost := m.Namespace[nsHostGroup].Value != "vm"
				if isHost { // Retrieve vShpere HOST metrics
					err := c.updateQuerySpecMap(ctx, hostQuerySpecs, defaultIntervalID, m.Namespace[nsHostGroup].Value, m.Namespace[nsHostMetric].Value, host.Name, host.Reference())
					if err != nil {
						return nil, err
					}
				} else { // Retrieve vShpere VIRTUAL MACHINE metrics
					// Retrieve VMs with name given in namespace entry
					vms, err := c.GovmomiResources.FindVMs(ctx, host, m.Namespace[nsVM].Value)
					if err != nil {
						return nil, err
					}
					for _, vm := range vms {
						err := c.updateQuerySpecMap(ctx, vmQuerySpecs, defaultIntervalID, m.Namespace[nsVMGroup].Value, m.Namespace[nsVMMetric].Value, vm.Name, vm.Reference())
						if err != nil {
							return nil, err
						}
					}
				}
			}
		} else if m.Namespace[nsSource].Value == "cluster" { // Retrieve cluster metrics
			cluster, err := c.GovmomiResources.GetCluster()
			if err != nil {
				return nil, err
			}

			err = c.updateQuerySpecMap(ctx, clusterQuerySpecs, 300, m.Namespace[nsClusterGroup].Value, m.Namespace[nsClusterMetric].Value, cluster.Name, cluster.Reference())
			if err != nil {
				return nil, err
			}
		}
	}

	for _, qs := range hostQuerySpecs {
		allQuerySpecs = append(allQuerySpecs, qs)
	}
	for _, qs := range vmQuerySpecs {
		allQuerySpecs = append(allQuerySpecs, qs)
	}
	for _, qs := range clusterQuerySpecs {
		allQuerySpecs = append(allQuerySpecs, qs)
	}

	if len(allQuerySpecs) == 0 {
		return nil, fmt.Errorf("cannot build query spec based on provided namespaces")
	}

	return allQuerySpecs, nil
}

// instanceToNs converts instance name to namespace entry
// As vSphere returns empty instance name for aggregated metrics, this function replaces it with predefined namespace entry
func (c *Collector) instanceToNs(instance string) string {
	if instance == "" {
		return aggregatedNs
	}
	return instance
}

// buildParsedQueryResponses parses API respose entity data, retrieves host name, vm name,
// counter name etc for each counter instance and stores discovered info in slice
func (c *Collector) buildParsedQueryResponses(ctx context.Context, entity types.BasePerfEntityMetricBase) ([]parsedQueryResponse, error) {
	result := []parsedQueryResponse{}

	instances, err := c.GovmomiResources.GetInstances(entity)
	if err != nil {
		return nil, err
	}

	// Loop through all metric instances
	for _, instance := range instances {
		// Retrieve instance counter info and value
		metric, err := c.GovmomiResources.GetInstanceSeries(instance)
		if err != nil {
			return nil, err
		}
		counter, err := c.GovmomiResources.FindCounterByKey(ctx, metric.Id.CounterId)
		if err != nil {
			return nil, err
		}
		counterGroup := counter.GroupInfo.GetElementDescription().Key
		counterName := counter.NameInfo.GetElementDescription().Key + "." + fmt.Sprint(counter.RollupType)

		entityType := entity.GetPerfEntityMetricBase().Entity.Type
		if entityType != "ClusterComputeResource" {
			if len(metric.Value) != 1 {
				return nil, fmt.Errorf("incorrect number of values (%d) for counter %s.%s", len(metric.Value), counterGroup, counterName)
			}
		}
		metricData := metric.Value[0]

		// Retrieve given entity info
		entityRef := entity.GetPerfEntityMetricBase().Entity.Reference()
		hostName := ""
		vmName := ""
		if entityType == "HostSystem" {
			host, err := c.GovmomiResources.FindHostByRef(ctx, entityRef)
			if err != nil {
				return nil, err
			}
			hostName = host.Name
		} else if entityType == "VirtualMachine" {
			vm, err := c.GovmomiResources.FindVMByRef(ctx, entityRef)
			if err != nil {
				return nil, err
			}
			vmHost, err := c.GovmomiResources.FindHostByRef(ctx, vm.Summary.Runtime.Host.Reference())
			if err != nil {
				return nil, err
			}
			hostName = vmHost.Name
			vmName = vm.Name
		} else if entityType == "ClusterComputeResource" {
			cluster, err := c.GovmomiResources.GetCluster()
			if err != nil {
				return nil, err
			}
			hostName = cluster.Name
			vmName = cluster.Name
		}

		// Append parsed response
		result = append(result, parsedQueryResponse{
			hostName:        hostName,
			vmName:          vmName,
			counterFullName: counterGroup + "." + counterName,
			instance:        c.instanceToNs(metric.Id.Instance),
			data:            metricData,
		})
	}
	return result, nil
}

// parsePerfQueryResponse converts raw perf query response to slice of structs which contain counter data, counter instance, host name and vm name
func (c *Collector) parsePerfQueryResponse(ctx context.Context, response *types.QueryPerfResponse) ([]parsedQueryResponse, error) {
	results := []parsedQueryResponse{}

	for _, entity := range response.Returnval {
		pqr, err := c.buildParsedQueryResponses(ctx, entity)
		if err != nil {
			return nil, err
		}
		results = append(results, pqr...)
	}

	return results, nil
}

// Filter parsed metrics based on given criteria
func (c *Collector) filterQuery(parsedQuery []parsedQueryResponse, hostName string, vmName string, counterFullNames []string, instance string) []parsedQueryResponse {
	result := []parsedQueryResponse{}
	for _, q := range parsedQuery {
		if (q.hostName == hostName || hostName == "*") &&
			(q.vmName == vmName || vmName == "*") {
			for _, counterFullName := range counterFullNames {
				if q.counterFullName == counterFullName {
					if q.instance == instance || instance == "*" {
						result = append(result, q)
					}
				}
			}
		}
	}
	return result
}

// CollectMetrics collects requested metrics
func (c *Collector) CollectMetrics(mts []plugin.Metric) ([]plugin.Metric, error) {
	if len(mts) < 1 {
		return nil, fmt.Errorf("No metrics specified")
	}

	metrics := []plugin.Metric{}
	ctx := context.Background()
	if err := c.GovmomiResources.Init(ctx, mts[0].Config); err != nil {
		return nil, fmt.Errorf("unable to initialize: %v", err)
	}

	c.GovmomiResources.ClearCache()

	// Build list of query specs to send in one packet
	querySpecs, err := c.buildQuerySpecsForMetrics(ctx, mts)
	if err != nil {
		return nil, err
	}

	// Retrieve metric data
	perfQuery, err := c.GovmomiResources.PerfQuery(ctx, querySpecs)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve query perf response: %v", err)
	}

	// Parse retrieved metric data (retrieve host name, vm name and instance id for each counter)
	results, err := c.parsePerfQueryResponse(ctx, perfQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to parse query perf response: %v", err)
	}

	// Convert retrieved metrics to snap namespaces
	for _, m := range mts {
		if m.Namespace[nsSource].Value == "host" {
			hostName := m.Namespace[nsHost].Value
			hosts, err := c.GovmomiResources.FindHosts(ctx, hostName)
			if err != nil {
				return nil, err
			}
			for _, host := range hosts {
				isHost := m.Namespace[nsHostGroup].Value != "vm"
				if isHost {
					hostGroup := m.Namespace[nsHostGroup].Value
					hostInstance := m.Namespace[nsHostInstance].Value
					hostMetric := m.Namespace[nsHostMetric].Value

					// Filter all counter values for host and instance given in namespace (both can be *)
					// Counter names for selected namespace are retrieved from metric dependency map
					hostValues := c.filterQuery(results, host.Name, "", metricDepMap[hostGroup][hostMetric], hostInstance)

					// Return host-level and multiple counter dependency metrics
					metric := plugin.Metric{
						Namespace: plugin.CopyNamespace(m.Namespace),
					}
					metric.Namespace[nsHost].Value = host.Name
					metric.Namespace[nsHostInstance].Value = aggregatedNs

					switch hostGroup + "." + hostMetric {
					case "mem.available":
						metric.Data = host.Hardware.MemorySize / unitMegabyte
						metrics = append(metrics, metric)
						continue
					}

					// Return counter-level host metrics
					for _, v := range hostValues {
						metric := plugin.Metric{
							Namespace: plugin.CopyNamespace(m.Namespace),
							Data:      v.data,
						}
						metric.Namespace[nsHost].Value = v.hostName
						metric.Namespace[nsHostInstance].Value = v.instance

						// Host derived metrics
						switch hostGroup + "." + hostMetric {

						// CPU idle
						case "cpu.wait":
							metric.Data = float64(v.data) / 100
						case "cpu.idle":
							metric.Data = 100 - float64(v.data)/100
						case "cpu.load":
							metric.Data = float64(v.data) / 100000

						// Memory metrics
						case "mem.usage":
							metric.Data = v.data / unitKilobyte
						case "mem.free":
							metric.Data = host.Hardware.MemorySize/unitMegabyte - v.data/unitKilobyte
						case "mem.swapUsage":
							metric.Data = v.data / unitKilobyte
						}

						metrics = append(metrics, metric)
					}
				} else {
					vmName := m.Namespace[nsVM].Value
					vmGroup := m.Namespace[nsVMGroup].Value
					vmInstance := m.Namespace[nsVMInstance].Value
					vmMetric := m.Namespace[nsVMMetric].Value

					vmValues := c.filterQuery(results, host.Name, vmName, metricDepMap[vmGroup][vmMetric], vmInstance)

					for _, v := range vmValues {
						metric := plugin.Metric{
							Namespace: plugin.CopyNamespace(m.Namespace),
							Data:      v.data,
						}
						metric.Namespace[nsHost].Value = v.hostName
						metric.Namespace[nsVM].Value = v.vmName
						metric.Namespace[nsVMInstance].Value = v.instance

						metrics = append(metrics, metric)
					}
				}
			}
		} else if m.Namespace[nsSource].Value == "cluster" {
			clusterGroup := m.Namespace[nsClusterGroup].Value
			clusterMetric := m.Namespace[nsClusterMetric].Value

			clusterValues := c.filterQuery(results, "*", "*", metricDepMap[clusterGroup][clusterMetric], "*")
			for _, v := range clusterValues {
				metric := plugin.Metric{
					Namespace: plugin.CopyNamespace(m.Namespace),
					Data:      v.data,
				}

				metric.Namespace[nsCluster].Value = v.hostName
				metric.Namespace[nsClusterInstance].Value = v.instance

				metrics = append(metrics, metric)
			}
		}
	}

	return metrics, nil
}

func (c *Collector) createDsNs(metric string) plugin.Namespace {
	return plugin.NewNamespace(vendor, class, name, "datastore").
		AddDynamicElement("datastore_name", "Name of dataqstore").
		AddDynamicElement("instance", "Metric instance ID").
		AddStaticElement(metric)
}

func (c *Collector) createHostNs(group string, metric string) plugin.Namespace {
	return plugin.NewNamespace(vendor, class, name, "host").
		AddDynamicElement("hostname", "Name of host, it can be IP address").
		AddStaticElement(group).
		AddDynamicElement("instance", "Metric instance ID").
		AddStaticElement(metric)
}

func (c *Collector) createVMNs(group string, metric string) plugin.Namespace {
	return plugin.NewNamespace(vendor, class, name, "host").
		AddDynamicElement("hostname", "Name of host, it can be IP address").
		AddStaticElement("vm").
		AddDynamicElement("vmname", "Name of virtual machine").
		AddStaticElement(group).
		AddDynamicElement("instance", "Metric instance ID").
		AddStaticElement(metric)
}

func (c *Collector) createClusterNs(metric string) plugin.Namespace {
	return plugin.NewNamespace(vendor, class, name, "cluster").
		AddDynamicElement("clustername", "Name of cluster").
		AddDynamicElement("instance", "Metric instance ID").
		AddStaticElement(metric)
}

// GetMetricTypes returns available metrics
func (c *Collector) GetMetricTypes(cfg plugin.Config) ([]plugin.Metric, error) {
	metrics := []plugin.Metric{}

	// HOST - MEMORY
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("mem", "usage"),
		Description: "Memory usage in megabytes",
		Unit:        "megabyte"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("mem", "free"),
		Description: "Free memory in megabytes",
		Unit:        "megabyte"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("mem", "available"),
		Description: "Available memory in megabytes",
		Unit:        "megabyte"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("mem", "swapUsage"),
		Description: "Sum of memory swapped of all powered on VMs and vSphere services on the host.",
		Unit:        "megabyte"})

	// HOST - CPU
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("cpu", "idle"),
		Description: "Total & per-core time CPU spent in an idle as a percentage during the last 20s",
		Unit:        "percent"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("cpu", "wait"),
		Description: "Total time CPU spent in a wait state as a percentage during the last 20s",
		Unit:        "percent"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("cpu", "load"),
		Description: "CPU load average over last 1 minute",
		Unit:        "number"})

	// HOST - NET
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("net", "kbrateTx"),
		Description: "Average amount of data transmitted per second during the last 20s",
		Unit:        "kiloBytesPerSecond"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("net", "kbrateRx"),
		Description: "Average amount of data received per second during the last 20s",
		Unit:        "kiloBytesPerSecond"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("net", "packetsTx"),
		Description: "Number of packets transmitted during the last 20s",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createHostNs("net", "packetsRx"),
		Description: "Number of packets received during the last 20s",
		Unit:        "number"})

	// VM - VIRTUALDISK
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createVMNs("virtualDisk", "readIops"),
		Description: "Read I/O operations per second",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createVMNs("virtualDisk", "writeIops"),
		Description: "Write I/O operations per second",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createVMNs("virtualDisk", "readThroughput"),
		Description: "Read throughput",
		Unit:        "kiloBytesPerSecond"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createVMNs("virtualDisk", "writeThroughput"),
		Description: "Write throughput",
		Unit:        "kiloBytesPerSecond"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createVMNs("virtualDisk", "readLatency"),
		Description: "Read latency",
		Unit:        "millisecond"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createVMNs("virtualDisk", "writeLatency"),
		Description: "Write latency",
		Unit:        "millisecond"})

	// CLUSTER
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("cpuCapacity"),
		Description: "CPU capacity",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("cpuUsage"),
		Description: "CPU usage",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("memCapacity"),
		Description: "Memory capacity",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("memUsage"),
		Description: "Memory usage",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("datastoreRead"),
		Description: "Datastore read rate",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("datastoreWrite"),
		Description: "Datastore write rate",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("netPacketsTx"),
		Description: "Network transmit rate",
		Unit:        "number"})
	metrics = append(metrics, plugin.Metric{
		Namespace:   c.createClusterNs("netPacketsRx"),
		Description: "Network receive rate",
		Unit:        "number"})

	return metrics, nil
}

// GetConfigPolicy retrieves config for the plugin
func (c *Collector) GetConfigPolicy() (plugin.ConfigPolicy, error) {
	policy := plugin.NewConfigPolicy()

	policy.AddNewStringRule([]string{vendor, class, name}, "url", true, plugin.SetDefaultString(""))
	policy.AddNewStringRule([]string{vendor, class, name}, "username", true, plugin.SetDefaultString(""))
	policy.AddNewStringRule([]string{vendor, class, name}, "password", true, plugin.SetDefaultString(""))
	policy.AddNewBoolRule([]string{vendor, class, name}, "insecure", false, plugin.SetDefaultBool(false))
	policy.AddNewStringRule([]string{vendor, class, name}, "clusterName", true, plugin.SetDefaultString(""))

	return *policy, nil
}
