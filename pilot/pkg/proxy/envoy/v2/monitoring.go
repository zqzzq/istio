// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package v2

import (
	"google.golang.org/grpc/codes"

	"istio.io/istio/pkg/mcp/status"
	"istio.io/pkg/monitoring"
)

var (
	errTag     = monitoring.MustCreateLabel("err")
	clusterTag = monitoring.MustCreateLabel("cluster")
	nodeTag    = monitoring.MustCreateLabel("node")
	typeTag    = monitoring.MustCreateLabel("type")

	cdsReject = monitoring.NewGauge(
		"pilot_xds_cds_reject",
		"Pilot rejected CSD configs.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	edsReject = monitoring.NewGauge(
		"pilot_xds_eds_reject",
		"Pilot rejected EDS.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	edsInstances = monitoring.NewGauge(
		"pilot_xds_eds_instances",
		"Instances for each cluster(grouped by locality), as of last push. Zero instances is an error.",
		monitoring.WithLabels(clusterTag),
	)

	edsAllLocalityEndpoints = monitoring.NewGauge(
		"pilot_xds_eds_all_locality_endpoints",
		"Network endpoints for each cluster(across all localities), as of last push. Zero endpoints is an error.",
		monitoring.WithLabels(clusterTag),
	)

	ldsReject = monitoring.NewGauge(
		"pilot_xds_lds_reject",
		"Pilot rejected LDS.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	rdsReject = monitoring.NewGauge(
		"pilot_xds_rds_reject",
		"Pilot rejected RDS.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	rdsExpiredNonce = monitoring.NewSum(
		"pilot_rds_expired_nonce",
		"Total number of RDS messages with an expired nonce.",
	)

	totalXDSRejects = monitoring.NewSum(
		"pilot_total_xds_rejects",
		"Total number of XDS responses from pilot rejected by proxy.",
	)

	monServices = monitoring.NewGauge(
		"pilot_services",
		"Total services known to pilot.",
	)

	// TODO: Update all the resource stats in separate routine
	// virtual services, destination rules, gateways, etc.
	xdsClients = monitoring.NewGauge(
		"pilot_xds",
		"Number of endpoints connected to this pilot using XDS.",
	)

	xdsResponseWriteTimeouts = monitoring.NewSum(
		"pilot_xds_write_timeout",
		"Pilot XDS response write timeouts.",
	)

	// Covers xds_builderr and xds_senderr for xds in {lds, rds, cds, eds}.
	pushes = monitoring.NewSum(
		"pilot_xds_pushes",
		"Pilot build and send errors for lds, rds, cds and eds.",
		monitoring.WithLabels(typeTag),
	)

	cdsPushes         = pushes.With(typeTag.Value("cds"))
	cdsSendErrPushes  = pushes.With(typeTag.Value("cds_senderr"))
	cdsBuildErrPushes = pushes.With(typeTag.Value("cds_builderr"))
	edsPushes         = pushes.With(typeTag.Value("eds"))
	edsSendErrPushes  = pushes.With(typeTag.Value("eds_senderr"))
	ldsPushes         = pushes.With(typeTag.Value("lds"))
	ldsSendErrPushes  = pushes.With(typeTag.Value("lds_senderr"))
	ldsBuildErrPushes = pushes.With(typeTag.Value("lds_builderr"))
	rdsPushes         = pushes.With(typeTag.Value("rds"))
	rdsSendErrPushes  = pushes.With(typeTag.Value("rds_senderr"))
	rdsBuildErrPushes = pushes.With(typeTag.Value("rds_builderr"))

	pushTime = monitoring.NewDistribution(
		"pilot_xds_push_time",
		"Total time in seconds Pilot takes to push lds, rds, cds and eds.",
		[]float64{.01, .1, 1, 3, 5, 10, 20, 30},
		monitoring.WithLabels(typeTag),
	)

	cdsPushTime = pushTime.With(typeTag.Value("cds"))
	edsPushTime = pushTime.With(typeTag.Value("eds"))
	ldsPushTime = pushTime.With(typeTag.Value("lds"))
	rdsPushTime = pushTime.With(typeTag.Value("rds"))

	// only supported dimension is millis, unfortunately. default to unitdimensionless.
	proxiesQueueTime = monitoring.NewDistribution(
		"pilot_proxy_queue_time",
		"Time in seconds, a proxy is in the push queue before being dequeued.",
		[]float64{.1, 1, 3, 5, 10, 20, 30},
	)

	// only supported dimension is millis, unfortunately. default to unitdimensionless.
	proxiesConvergeDelay = monitoring.NewDistribution(
		"pilot_proxy_convergence_time",
		"Delay in seconds between config change and a proxy receiving all required configuration.",
		[]float64{.1, .5, 1, 3, 5, 10, 20, 30},
	)

	pushContextErrors = monitoring.NewSum(
		"pilot_xds_push_context_errors",
		"Number of errors (timeouts) initiating push context.",
	)

	totalXDSInternalErrors = monitoring.NewSum(
		"pilot_total_xds_internal_errors",
		"Total number of internal XDS errors in pilot.",
	)

	inboundUpdates = monitoring.NewSum(
		"pilot_inbound_updates",
		"Total number of updates received by pilot.",
		monitoring.WithLabels(typeTag),
	)

	inboundConfigUpdates  = inboundUpdates.With(typeTag.Value("config"))
	inboundEDSUpdates     = inboundUpdates.With(typeTag.Value("eds"))
	inboundServiceUpdates = inboundUpdates.With(typeTag.Value("svc"))
	inboundServiceDeletes = inboundUpdates.With(typeTag.Value("svcdelete"))
)

func recordSendError(metric monitoring.Metric, err error) {
	s, ok := status.FromError(err)
	// Unavailable or canceled code will be sent when a connection is closing down. This is very normal,
	// due to the XDS connection being dropped every 30 minutes, or a pod shutting down.
	isError := s.Code() != codes.Unavailable && s.Code() != codes.Canceled
	if !ok || isError {
		metric.Increment()
	}
}

func incrementXDSRejects(metric monitoring.Metric, node, errCode string) {
	metric.With(nodeTag.Value(node), errTag.Value(errCode)).Increment()
	totalXDSRejects.Increment()
}

func init() {
	monitoring.MustRegister(
		cdsReject,
		edsReject,
		ldsReject,
		rdsReject,
		edsInstances,
		edsAllLocalityEndpoints,
		rdsExpiredNonce,
		totalXDSRejects,
		monServices,
		xdsClients,
		xdsResponseWriteTimeouts,
		pushes,
		pushTime,
		proxiesConvergeDelay,
		proxiesQueueTime,
		pushContextErrors,
		totalXDSInternalErrors,
		inboundUpdates,
	)
}
