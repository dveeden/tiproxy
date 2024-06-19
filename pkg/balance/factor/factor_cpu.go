// Copyright 2024 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package factor

import (
	"math"
	"time"

	"github.com/pingcap/tiproxy/lib/config"
	"github.com/pingcap/tiproxy/pkg/balance/metricsreader"
	"github.com/pingcap/tiproxy/pkg/util/monotime"
	"github.com/prometheus/common/model"
)

const (
	cpuEwmaAlpha = 0.5
	// If some metrics are missing, we use the old one temporarily for no longer than cpuMetricExpDuration.
	cpuMetricExpDuration = 2 * time.Minute
	cpuScoreStep         = 5
	// 0.001 represents for 0.1%
	minCpuPerConn    = 0.001
	cpuBalancedRatio = 1.2
	// If the CPU difference of 2 backends is 30% and we're narrowing it to 20% in 30 seconds,
	// then in each round, we migrate ((30% - 20%) / 2) / usagePerConn / 30 = 1 / usagePerConn / 600 connections.
	balanceRatio4Cpu = 600
)

var _ Factor = (*FactorCPU)(nil)

var (
	cpuQueryExpr = metricsreader.QueryExpr{
		PromQL:   `irate(process_cpu_seconds_total{%s="tidb"}[30s])/tidb_server_maxprocs`,
		HasLabel: true,
		Range:    1 * time.Minute,
	}
)

type cpuBackendSnapshot struct {
	updatedTime monotime.Time
	// smoothed CPU usage, used to decide whether to migrate
	avgUsage float64
	// timely CPU usage, used to score and decide the balance count
	latestUsage float64
	connCount   int
}

type FactorCPU struct {
	// The snapshot of backend statistics when the matrix was updated.
	snapshot map[string]cpuBackendSnapshot
	// The updated time of the metric that we've read last time.
	lastMetricTime monotime.Time
	// The estimated average CPU usage used by one connection.
	usagePerConn float64
	mr           metricsreader.MetricsReader
	queryID      uint64
	bitNum       int
}

func NewFactorCPU(mr metricsreader.MetricsReader) *FactorCPU {
	return &FactorCPU{
		mr:       mr,
		queryID:  mr.AddQueryExpr(cpuQueryExpr),
		bitNum:   5,
		snapshot: make(map[string]cpuBackendSnapshot),
	}
}

func (fc *FactorCPU) Name() string {
	return "cpu"
}

func (fc *FactorCPU) UpdateScore(backends []scoredBackend) {
	if len(backends) <= 1 {
		return
	}
	qr := fc.mr.GetQueryResult(fc.queryID)
	if qr.Err != nil || qr.Empty() {
		return
	}

	if qr.UpdateTime != fc.lastMetricTime {
		// Metrics have updated.
		fc.lastMetricTime = qr.UpdateTime
		fc.updateSnapshot(qr, backends)
		fc.updateCpuPerConn()
	}
	if monotime.Since(fc.lastMetricTime) > cpuMetricExpDuration {
		// The metrics have not been updated for a long time (maybe Prometheus is unavailable).
		return
	}

	for i := 0; i < len(backends); i++ {
		_, latestUsage := fc.getUsage(backends[i])
		backends[i].addScore(int(latestUsage*100)/cpuScoreStep, fc.bitNum)
	}
}

func (fc *FactorCPU) updateSnapshot(qr metricsreader.QueryResult, backends []scoredBackend) {
	snapshots := make(map[string]cpuBackendSnapshot, len(fc.snapshot))
	for _, backend := range backends {
		addr := backend.Addr()
		valid := false
		// If a backend exists in metrics but not in the backend list, ignore it for this round.
		// The backend will be in the next round if it's healthy.
		pairs := qr.GetSamplePair4Backend(backend)
		if len(pairs) > 0 {
			avgUsage, latestUsage := calcAvgUsage(pairs)
			if avgUsage >= 0 {
				snapshots[addr] = cpuBackendSnapshot{
					avgUsage:    avgUsage,
					latestUsage: latestUsage,
					connCount:   backend.ConnCount(),
					updatedTime: qr.UpdateTime,
				}
				valid = true
			}
		}
		// Merge the old snapshot just in case some metrics have missed for a short period.
		if !valid {
			if snapshot, ok := fc.snapshot[addr]; ok {
				if monotime.Since(snapshot.updatedTime) < cpuMetricExpDuration {
					snapshots[addr] = snapshot
				}
			}
		}
	}
	fc.snapshot = snapshots
}

func calcAvgUsage(usageHistory []model.SamplePair) (avgUsage, latestUsage float64) {
	avgUsage, latestUsage = -1, -1
	if len(usageHistory) == 0 {
		return
	}
	// The CPU usage may jitter, so use the EWMA algorithm to make it smooth.
	for _, usage := range usageHistory {
		value := float64(usage.Value)
		if math.IsNaN(value) {
			continue
		}
		latestUsage = value
		if avgUsage < 0 {
			avgUsage = value
		} else {
			avgUsage = avgUsage*(1-cpuEwmaAlpha) + value*cpuEwmaAlpha
		}
	}
	if avgUsage > 1 {
		avgUsage = 1
	}
	return
}

// Estimate the average CPU usage used by one connection.
// Don't fetch the global connection count (tidb_server_connections) because the connScore is also based on the local count.
// Don't estimate it based on each backend because background jobs may use much CPU.
// E.g. auto-analyze uses 30% CPU and the backend has 1 connection. You may mistakenly think the connection uses 30% CPU.
func (fc *FactorCPU) updateCpuPerConn() {
	totalUsage, totalConns := 0.0, 0
	for _, backend := range fc.snapshot {
		if backend.latestUsage > 0 && backend.connCount > 0 {
			totalUsage += backend.latestUsage
			totalConns += backend.connCount
		}
	}
	if totalConns > 0 {
		usagePerConn := totalUsage / float64(totalConns)
		// When the cluster is idle and the clients are connecting to it all at once (e.g. when sysbench starts),
		// the CPU usage lags behind, so the usagePerConn may be very low. In this case, all the connections may be
		// routed to the same backend just because the CPU usage of the backend is a little lower.
		if usagePerConn < minCpuPerConn {
			// If the average usage is below 10%, we take the cluster as just started and don't update usagePerConn.
			if totalUsage/float64(len(fc.snapshot)) > 0.1 {
				fc.usagePerConn = usagePerConn
			}
		} else {
			fc.usagePerConn = usagePerConn
		}
	}
	if fc.usagePerConn <= 0 {
		fc.usagePerConn = minCpuPerConn
	}
}

// Estimate the current cpu usage by the latest CPU usage, the latest connection count, and the current connection count.
func (fc *FactorCPU) getUsage(backend scoredBackend) (avgUsage, latestUsage float64) {
	snapshot, ok := fc.snapshot[backend.Addr()]
	if !ok || snapshot.avgUsage < 0 || latestUsage < 0 {
		// The metric has missed for minutes.
		return 1, 1
	}
	avgUsage = snapshot.avgUsage
	latestUsage = snapshot.latestUsage + float64(backend.ConnScore()-snapshot.connCount)*fc.usagePerConn
	if latestUsage > 1 {
		latestUsage = 1
	}
	return
}

func (fc *FactorCPU) ScoreBitNum() int {
	return fc.bitNum
}

func (fc *FactorCPU) BalanceCount(from, to scoredBackend) int {
	fromAvgUsage, fromLatestUsage := fc.getUsage(from)
	toAvgUsage, toLatestUsage := fc.getUsage(to)
	// The higher the CPU usage, the more sensitive the load balance should be.
	// E.g. 10% vs 25% don't need rebalance, but 80% vs 95% need rebalance.
	// Use the average usage to avoid thrash when CPU jitters too much and use the latest usage to avoid migrate too many connections.
	if 1.3-toAvgUsage > (1.3-fromAvgUsage)*cpuBalancedRatio && 1.3-toLatestUsage > (1.3-fromLatestUsage)*cpuBalancedRatio {
		if balanceCount := int(1 / fc.usagePerConn / balanceRatio4Cpu); balanceCount > 1 {
			return balanceCount
		}
		return 1
	}
	return 0
}

func (fc *FactorCPU) SetConfig(cfg *config.Config) {
}

func (fc *FactorCPU) Close() {
	fc.mr.RemoveQueryExpr(fc.queryID)
}
