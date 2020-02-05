package costmodel

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/kubecost/cost-model/cloud"
	"github.com/kubecost/cost-model/util"
	prometheus "github.com/prometheus/client_golang/api"
	"k8s.io/klog"
)

const (
	queryClusterCores = `sum(
		avg(avg_over_time(kube_node_status_capacity_cpu_cores[%s] %s)) by (node, cluster_id) * avg(avg_over_time(node_cpu_hourly_cost[%s] %s)) by (node, cluster_id) * 730 +
		avg(avg_over_time(node_gpu_hourly_cost[%s] %s)) by (node, cluster_id) * 730
	  ) by (cluster_id)`

	queryClusterRAM = `sum(
		avg(avg_over_time(kube_node_status_capacity_memory_bytes[%s] %s)) by (node, cluster_id) / 1024 / 1024 / 1024 * avg(avg_over_time(node_ram_hourly_cost[%s] %s)) by (node, cluster_id) * 730
	  ) by (cluster_id)`

	queryStorage = `sum(
		avg(avg_over_time(pv_hourly_cost[%s] %s)) by (persistentvolume, cluster_id) * 730 
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes[%s] %s)) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024
	  ) by (cluster_id) %s`

	queryTotal = `sum(avg(node_total_hourly_cost) by (node, cluster_id)) * 730 +
	  sum(
		avg(avg_over_time(pv_hourly_cost[1h])) by (persistentvolume, cluster_id) * 730 
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes[1h])) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024
	  ) by (cluster_id) %s`
)

// TODO move this to a package-accessible helper
type PromQueryContext struct {
	client prometheus.Client
	ec     *util.ErrorCollector
	wg     *sync.WaitGroup
}

// TODO move this to a package-accessible helper function once dependencies are able to
// be extricated from costmodel package (PromQueryResult -> Vector). Otherwise, circular deps.
func AsyncPromQuery(query string, resultCh chan []*PromQueryResult, ctx PromQueryContext) {
	if ctx.wg != nil {
		defer ctx.wg.Done()
	}

	raw, promErr := Query(ctx.client, query)
	ctx.ec.Report(promErr)

	results, parseErr := NewQueryResults(raw)
	ctx.ec.Report(parseErr)

	resultCh <- results
}

// Costs represents cumulative and monthly cluster costs over a given duration. Costs
// are broken down by cores, memory, and storage.
type ClusterCosts struct {
	Start             *time.Time `json:"startTime"`
	End               *time.Time `json:"endTime"`
	CPUCumulative     float64    `json:"cpuCumulativeCost"`
	CPUMonthly        float64    `json:"cpuMonthlyCost"`
	RAMCumulative     float64    `json:"ramCumulativeCost"`
	RAMMonthly        float64    `json:"ramMonthlyCost"`
	StorageCumulative float64    `json:"storageCumulativeCost"`
	StorageMonthly    float64    `json:"storageMonthlyCost"`
	TotalCumulative   float64    `json:"totalCost"`
	TotalMonthly      float64    `json:"totalMonthlyCost"`
}

// NewClusterCostsFromCumulative takes cumulative cost data over a given time range, computes
// the associated monthly rate data, and returns the Costs.
func NewClusterCostsFromCumulative(cpu, ram, storage float64, window, offset string) (*ClusterCosts, error) {
	start, end, err := util.ParseTimeRange(window, offset)
	if err != nil {
		return nil, err
	}

	hours := end.Sub(*start).Hours()

	cc := &ClusterCosts{
		Start:             start,
		End:               end,
		CPUCumulative:     cpu,
		RAMCumulative:     ram,
		StorageCumulative: storage,
		TotalCumulative:   cpu + ram + storage,
		CPUMonthly:        cpu / hours * (util.HoursPerDay * util.DaysPerMonth),
		RAMMonthly:        ram / hours * (util.HoursPerDay * util.DaysPerMonth),
		StorageMonthly:    storage / hours * (util.HoursPerDay * util.DaysPerMonth),
	}
	cc.TotalMonthly = cc.CPUMonthly + cc.RAMMonthly + cc.StorageMonthly

	return cc, nil
}

// NewClusterCostsFromMonthly takes monthly-rate cost data over a given time range, computes
// the associated cumulative cost data, and returns the Costs.
func NewClusterCostsFromMonthly(cpuMonthly, ramMonthly, storageMonthly float64, window, offset string) (*ClusterCosts, error) {
	start, end, err := util.ParseTimeRange(window, offset)
	if err != nil {
		return nil, err
	}

	hours := end.Sub(*start).Hours()

	cc := &ClusterCosts{
		Start:             start,
		End:               end,
		CPUMonthly:        cpuMonthly,
		RAMMonthly:        ramMonthly,
		StorageMonthly:    storageMonthly,
		TotalMonthly:      cpuMonthly + ramMonthly + storageMonthly,
		CPUCumulative:     cpuMonthly / util.HoursPerMonth * hours,
		RAMCumulative:     ramMonthly / util.HoursPerMonth * hours,
		StorageCumulative: storageMonthly / util.HoursPerMonth * hours,
	}
	cc.TotalCumulative = cc.CPUCumulative + cc.RAMCumulative + cc.StorageCumulative

	return cc, nil
}

// ComputeClusterCosts gives the cumulative and monthly-rate cluster costs over a window of time for all clusters.
func ComputeClusterCosts(client prometheus.Client, provider cloud.Provider, window, offset string) (map[string]*ClusterCosts, error) {
	const fmtQueryTotalCPU = `sum(
		sum(sum_over_time(kube_node_status_capacity_cpu_cores[%s:1h]%s)) by (node, cluster_id) *
		avg(avg_over_time(node_cpu_hourly_cost[%s:1h]%s)) by (node, cluster_id)
	)`

	const fmtQueryTotalRAM = `sum(
		sum(sum_over_time(kube_node_status_capacity_memory_bytes[%s:1h]%s) / 1024 / 1024 / 1024) by (node, cluster_id) *
		avg(avg_over_time(node_ram_hourly_cost[%s:1h]%s)) by (node, cluster_id)
	)`

	const fmtQueryTotalStorage = `sum(
		sum(sum_over_time(kube_persistentvolume_capacity_bytes[%s:1h]%s)) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024 *
		avg(avg_over_time(pv_hourly_cost[%s:1h]%s)) by (persistentvolume, cluster_id)
	)`

	// TODO local storage

	// TODO norm for interpolating missed scrapes?

	fmtOffset := ""
	if offset != "" {
		fmtOffset = fmt.Sprintf("offset %s", offset)
	}

	queryTotalCPU := fmt.Sprintf(fmtQueryTotalCPU, window, fmtOffset, window, fmtOffset)
	queryTotalRAM := fmt.Sprintf(fmtQueryTotalRAM, window, fmtOffset, window, fmtOffset)
	queryTotalStorage := fmt.Sprintf(fmtQueryTotalStorage, window, fmtOffset, window, fmtOffset)
	numQueries := 3

	klog.V(4).Infof("[Debug] queryTotalCPU: %s", queryTotalCPU)
	klog.V(4).Infof("[Debug] queryTotalRAM: %s", queryTotalRAM)
	klog.V(4).Infof("[Debug] queryTotalStorage: %s", queryTotalStorage)

	// Submit queries to Prometheus asynchronously
	var ec util.ErrorCollector
	var wg sync.WaitGroup
	ctx := PromQueryContext{client, &ec, &wg}
	ctx.wg.Add(numQueries)

	chTotalCPU := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalCPU, chTotalCPU, ctx)

	chTotalRAM := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalRAM, chTotalRAM, ctx)

	chTotalStorage := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalStorage, chTotalStorage, ctx)

	// After queries complete, retrieve results
	wg.Wait()

	resultsTotalCPU := <-chTotalCPU
	close(chTotalCPU)

	resultsTotalRAM := <-chTotalRAM
	close(chTotalRAM)

	resultsTotalStorage := <-chTotalStorage
	close(chTotalStorage)

	// Intermediate structure storing mapping of [clusterID][type ∈ {cpu, ram, storage, total}]=cost
	costData := make(map[string]map[string]float64)
	defaultClusterID := os.Getenv(clusterIDKey)

	// Helper function to iterate over Prom query results, parsing the raw values into
	// the intermediate costData structure.
	setCostsFromResults := func(costData map[string]map[string]float64, results []*PromQueryResult, name string) {
		for _, result := range results {
			clusterID, _ := result.GetString("cluster_id")
			if clusterID == "" {
				clusterID = defaultClusterID
			}
			if _, ok := costData[clusterID]; !ok {
				costData[clusterID] = map[string]float64{}
			}
			if len(result.Values) > 0 {
				costData[clusterID][name] += result.Values[0].Value
				costData[clusterID]["total"] += result.Values[0].Value
			}
		}
	}
	setCostsFromResults(costData, resultsTotalCPU, "cpu")
	setCostsFromResults(costData, resultsTotalRAM, "ram")
	setCostsFromResults(costData, resultsTotalStorage, "storage")

	// Convert intermediate structure to Costs instances
	costsByCluster := map[string]*ClusterCosts{}
	for id, cd := range costData {
		costs, err := NewClusterCostsFromCumulative(cd["cpu"], cd["ram"], cd["storage"], window, offset)
		if err != nil {
			klog.V(3).Infof("[Warning] Failed to parse cluster costs on %s (%s) from cumulative data: %+v", window, offset, cd)
			return nil, err
		}
		costsByCluster[id] = costs
	}

	return costsByCluster, nil
}

type Totals struct {
	TotalCost   [][]string `json:"totalcost"`
	CPUCost     [][]string `json:"cpucost"`
	MemCost     [][]string `json:"memcost"`
	StorageCost [][]string `json:"storageCost"`
}

func resultToTotals(qr interface{}) ([][]string, error) {
	results, err := NewQueryResults(qr)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("Not enough data available in the selected time range")
	}

	result := results[0]
	totals := [][]string{}
	for _, value := range result.Values {
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		totals = append(totals, toAppend)
	}
	return totals, nil
}

func resultToTotal(qr interface{}) (map[string][][]string, error) {
	defaultClusterID := os.Getenv(clusterIDKey)

	results, err := NewQueryResults(qr)
	if err != nil {
		return nil, err
	}

	toReturn := make(map[string][][]string)
	for _, result := range results {
		clusterID, _ := result.GetString("cluster_id")
		if clusterID == "" {
			clusterID = defaultClusterID
		}

		// Expect a single value only
		if len(result.Values) == 0 {
			klog.V(1).Infof("[Warning] Metric values did not contain any valid data.")
			continue
		}

		value := result.Values[0]
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		if t, ok := toReturn[clusterID]; ok {
			t = append(t, toAppend)
		} else {
			toReturn[clusterID] = [][]string{toAppend}
		}
	}

	return toReturn, nil
}

// ClusterCostsForAllClusters gives the cluster costs averaged over a window of time for all clusters.
func ClusterCostsForAllClusters(cli prometheus.Client, cloud cloud.Provider, window, offset string) (map[string]*Totals, error) {
	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	localStorageQuery, err := cloud.GetLocalStorageQuery(offset)
	if err != nil {
		return nil, err
	}
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	qCores := fmt.Sprintf(queryClusterCores, window, offset, window, offset, window, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, window, offset, window, offset)
	qStorage := fmt.Sprintf(queryStorage, window, offset, window, offset, localStorageQuery)

	klog.V(4).Infof("Running query %s", qCores)
	resultClusterCores, err := Query(cli, qCores)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qCores, err.Error())
	}

	klog.V(4).Infof("Running query %s", qRAM)
	resultClusterRAM, err := Query(cli, qRAM)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qRAM, err.Error())
	}

	klog.V(4).Infof("Running query %s", qRAM)
	resultStorage, err := Query(cli, qStorage)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qStorage, err.Error())
	}

	toReturn := make(map[string]*Totals)

	coreTotal, err := resultToTotal(resultClusterCores)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qCores, err.Error())
	}
	for clusterID, total := range coreTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].CPUCost = total
	}

	ramTotal, err := resultToTotal(resultClusterRAM)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qRAM, err.Error())
	}
	for clusterID, total := range ramTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].MemCost = total
	}

	storageTotal, err := resultToTotal(resultStorage)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qStorage, err.Error())
	}
	for clusterID, total := range storageTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].StorageCost = total
	}

	return toReturn, nil
}

// AverageClusterTotals gives the current full cluster costs averaged over a window of time.
// Used to be ClutserCosts, but has been deprecated for that use.
func AverageClusterTotals(cli prometheus.Client, cloud cloud.Provider, windowString, offset string) (*Totals, error) {
	// turn offsets of the format "[0-9+]h" into the format "offset [0-9+]h" for use in query templatess
	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	localStorageQuery, err := cloud.GetLocalStorageQuery(offset)
	if err != nil {
		return nil, err
	}
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, offset, windowString, offset, windowString, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, offset, windowString, offset)
	qStorage := fmt.Sprintf(queryStorage, windowString, offset, windowString, offset, localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, localStorageQuery)

	resultClusterCores, err := Query(cli, qCores)
	if err != nil {
		return nil, err
	}
	resultClusterRAM, err := Query(cli, qRAM)
	if err != nil {
		return nil, err
	}

	resultStorage, err := Query(cli, qStorage)
	if err != nil {
		return nil, err
	}

	resultTotal, err := Query(cli, qTotal)
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotal(resultClusterCores)
	if err != nil {
		return nil, err
	}

	ramTotal, err := resultToTotal(resultClusterRAM)
	if err != nil {
		return nil, err
	}

	storageTotal, err := resultToTotal(resultStorage)
	if err != nil {
		return nil, err
	}

	clusterTotal, err := resultToTotal(resultTotal)
	if err != nil {
		return nil, err
	}

	defaultClusterID := os.Getenv(clusterIDKey)

	return &Totals{
		TotalCost:   clusterTotal[defaultClusterID],
		CPUCost:     coreTotal[defaultClusterID],
		MemCost:     ramTotal[defaultClusterID],
		StorageCost: storageTotal[defaultClusterID],
	}, nil
}

// ClusterCostsOverTime gives the full cluster costs over time
func ClusterCostsOverTime(cli prometheus.Client, cloud cloud.Provider, startString, endString, windowString, offset string) (*Totals, error) {

	localStorageQuery, err := cloud.GetLocalStorageQuery(offset)
	if err != nil {
		return nil, err
	}
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	layout := "2006-01-02T15:04:05.000Z"

	start, err := time.Parse(layout, startString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + startString + ". Error: " + err.Error())
		return nil, err
	}
	end, err := time.Parse(layout, endString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + endString + ". Error: " + err.Error())
		return nil, err
	}
	window, err := time.ParseDuration(windowString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + windowString + ". Error: " + err.Error())
		return nil, err
	}

	// turn offsets of the format "[0-9+]h" into the format "offset [0-9+]h" for use in query templatess
	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, offset, windowString, offset, windowString, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, offset, windowString, offset)
	qStorage := fmt.Sprintf(queryStorage, windowString, offset, windowString, offset, localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, localStorageQuery)

	resultClusterCores, err := QueryRange(cli, qCores, start, end, window)
	if err != nil {
		return nil, err
	}
	resultClusterRAM, err := QueryRange(cli, qRAM, start, end, window)
	if err != nil {
		return nil, err
	}

	resultStorage, err := QueryRange(cli, qStorage, start, end, window)
	if err != nil {
		return nil, err
	}

	resultTotal, err := QueryRange(cli, qTotal, start, end, window)
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotals(resultClusterCores)
	if err != nil {
		return nil, err
	}

	ramTotal, err := resultToTotals(resultClusterRAM)
	if err != nil {
		return nil, err
	}

	storageTotal, err := resultToTotals(resultStorage)
	if err != nil {
		return nil, err
	}

	clusterTotal, err := resultToTotals(resultTotal)
	if err != nil {
		return nil, err
	}

	return &Totals{
		TotalCost:   clusterTotal,
		CPUCost:     coreTotal,
		MemCost:     ramTotal,
		StorageCost: storageTotal,
	}, nil

}
