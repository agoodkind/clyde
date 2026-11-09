package daemon

import (
	"slices"
	"strings"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

func aggregateMetricsRequests(
	report *MetricsHistoryReport,
	requests map[string]*metricsRequest,
	pricing adapterruntime.PricingTable,
) {
	durationSeconds := report.Window.Until.Sub(report.Window.Since).Seconds()
	totalSamples := make([]int64, 0, len(requests))
	stageSamples := make(map[string][]int64)
	unknownModels := make(map[string]bool)

	deltas := metricsCounterDeltas{
		requests:     0,
		completed:    0,
		failed:       0,
		cancelled:    0,
		bytesIn:      0,
		bytesOut:     0,
		inputTokens:  0,
		outputTokens: 0,
		cacheTokens:  0,
		cost:         0,
	}
	for _, request := range requests {
		if request.invalidLifecycle {
			continue
		}

		if !request.terminal {
			continue
		}

		if request.terminalAt.Before(report.Window.Since) ||
			request.terminalAt.After(report.Window.Until) {
			continue
		}

		if !request.started {
			report.invalidHistory = true
			addMetricsWarning(report, "request terminal record has no retained start")
		}

		deltas.requests++
		deltas.bytesIn += request.bytesIn
		deltas.bytesOut += request.bytesOut
		deltas.inputTokens += request.promptTokens
		deltas.outputTokens += request.outputTokens
		deltas.cacheTokens += request.cacheTokens
		addRequestStatusDelta(&deltas, request.status)
		cost, unknownModel := priceMetricsRequest(request, pricing)
		deltas.cost += cost
		if unknownModel != "" {
			unknownModels[unknownModel] = true
		}

		addRequestDuration(report, request, &totalSamples, stageSamples)
	}

	if report.sawCanonicalRecord {
		setMetricCounter(&report.Metrics.Requests, deltas.requests, durationSeconds)
		setMetricCounter(&report.Metrics.Completed, deltas.completed, durationSeconds)
		setMetricCounter(&report.Metrics.Failed, deltas.failed, durationSeconds)
		setMetricCounter(&report.Metrics.Cancelled, deltas.cancelled, durationSeconds)
		setMetricCounter(&report.Metrics.BytesIn, deltas.bytesIn, durationSeconds)
		setMetricCounter(&report.Metrics.BytesOut, deltas.bytesOut, durationSeconds)
		setMetricCounter(&report.Metrics.InputTokens, deltas.inputTokens, durationSeconds)
		setMetricCounter(&report.Metrics.OutputTokens, deltas.outputTokens, durationSeconds)
		setMetricCounter(&report.Metrics.CacheTokens, deltas.cacheTokens, durationSeconds)
		if len(unknownModels) == 0 {
			setMetricCounter(&report.Metrics.CostMicrocents, deltas.cost, durationSeconds)
		}
	}

	unknownModelNames := make([]string, 0, len(unknownModels))
	for model := range unknownModels {
		unknownModelNames = append(unknownModelNames, model)
	}

	slices.Sort(unknownModelNames)

	for _, model := range unknownModelNames {
		addMetricsWarning(report, "pricing unavailable for model "+model)
	}

	report.TimeBreakdown.Total = durationFromSamples(totalSamples)
	report.TimeBreakdown.Stages = rankedStageDurations(stageSamples)
	inflightSamples, streamingSamples := lifecycleGaugeSamples(requests, report.Window)
	report.Metrics.Inflight = gaugeFromSamples(inflightSamples)
	report.Metrics.Streaming = gaugeFromSamples(streamingSamples)
	if len(totalSamples) > 0 && report.UnattributedDurationMS == nil {
		report.UnattributedDurationMS = new(int64(0))
	}
}

func addRequestStatusDelta(deltas *metricsCounterDeltas, status metricsRequestStatus) {
	switch status {
	case metricsRequestStatusCompleted:
		deltas.completed++
	case metricsRequestStatusFailed:
		deltas.failed++
	case metricsRequestStatusCancelled:
		deltas.cancelled++
	default:
		deltas.completed++
	}
}

func priceMetricsRequest(
	request *metricsRequest,
	pricing adapterruntime.PricingTable,
) (int64, string) {
	if request.promptTokens == 0 && request.outputTokens == 0 && request.cacheTokens == 0 {
		return 0, ""
	}

	cost := adapterruntime.EstimateCost(
		adapterruntime.CostInputs{
			ModelID:             request.model,
			TTL:                 "",
			InputTokens:         int(request.promptTokens),
			OutputTokens:        int(request.outputTokens),
			CacheCreationTokens: int(request.cacheCreationTokens),
			CacheReadTokens:     int(request.cacheReadTokens),
		},
		pricing,
	)
	if cost.RatesKnown {
		return cost.TotalMicrocents, ""
	}

	model := strings.TrimSpace(request.model)
	if model == "" {
		model = "<missing>"
	}

	return 0, model
}
