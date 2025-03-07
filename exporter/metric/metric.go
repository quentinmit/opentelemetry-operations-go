// Copyright 2021 Google LLC
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

package metric

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"go.uber.org/multierr"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/distribution"
	"google.golang.org/genproto/googleapis/api/label"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/googleapis/gax-go/v2"

	"github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping"
)

const (
	// The number of timeserieses to send to GCM in a single request. This
	// is a hard limit in the GCM API, so we never want to exceed 200.
	sendBatchSize = 200

	cloudMonitoringMetricDescriptorNameFormat = "workload.googleapis.com/%s"
)

// key is used to judge the uniqueness of the record descriptor.
type key struct {
	name        string
	libraryname string
}

func keyOf(metrics metricdata.Metrics, library instrumentation.Library) key {
	return key{
		name:        metrics.Name,
		libraryname: library.Name,
	}
}

// metricExporter is the implementation of OpenTelemetry metric exporter for
// Google Cloud Monitoring.
type metricExporter struct {
	o        *options
	shutdown chan struct{}
	// mdCache is the cache to hold MetricDescriptor to avoid creating duplicate MD.
	mdCache      map[key]*googlemetricpb.MetricDescriptor
	client       *monitoring.MetricClient
	mdLock       sync.RWMutex
	shutdownOnce sync.Once
}

// ForceFlush does nothing, the exporter holds no state.
func (e *metricExporter) ForceFlush(ctx context.Context) error { return ctx.Err() }

// Shutdown shuts down the client connections.
func (e *metricExporter) Shutdown(ctx context.Context) error {
	err := errShutdown
	e.shutdownOnce.Do(func() {
		close(e.shutdown)
		err = multierr.Combine(ctx.Err(), e.client.Close())
	})
	return err
}

// newMetricExporter returns an exporter that uploads OTel metric data to Google Cloud Monitoring.
func newMetricExporter(o *options) (*metricExporter, error) {
	if strings.TrimSpace(o.projectID) == "" {
		return nil, errBlankProjectID
	}

	clientOpts := append([]option.ClientOption{option.WithUserAgent(userAgent)}, o.monitoringClientOptions...)
	ctx := o.context
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := monitoring.NewMetricClient(ctx, clientOpts...)
	if err != nil {
		return nil, err
	}

	if o.compression == "gzip" {
		client.CallOptions.GetMetricDescriptor = append(client.CallOptions.CreateMetricDescriptor,
			gax.WithGRPCOptions(grpc.UseCompressor(gzip.Name)))
		client.CallOptions.CreateMetricDescriptor = append(client.CallOptions.CreateMetricDescriptor,
			gax.WithGRPCOptions(grpc.UseCompressor(gzip.Name)))
		client.CallOptions.CreateTimeSeries = append(client.CallOptions.CreateTimeSeries,
			gax.WithGRPCOptions(grpc.UseCompressor(gzip.Name)))
	}

	cache := map[key]*googlemetricpb.MetricDescriptor{}
	e := &metricExporter{
		o:        o,
		mdCache:  cache,
		client:   client,
		shutdown: make(chan struct{}),
	}
	return e, nil
}

var errShutdown = fmt.Errorf("exporter is shutdown")

// Export exports OpenTelemetry Metrics to Google Cloud Monitoring.
func (me *metricExporter) Export(ctx context.Context, rm metricdata.ResourceMetrics) error {
	select {
	case <-me.shutdown:
		return errShutdown
	default:
	}

	if me.o.destinationProjectQuota {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{"x-goog-user-project": strings.TrimPrefix(me.o.projectID, "projects/")}))
	}
	return multierr.Combine(
		me.exportMetricDescriptor(ctx, rm),
		me.exportTimeSeries(ctx, rm),
	)
}

// Temporality returns the Temporality to use for an instrument kind.
func (me *metricExporter) Temporality(ik metric.InstrumentKind) metricdata.Temporality {
	return metric.DefaultTemporalitySelector(ik)
}

// Aggregation returns the Aggregation to use for an instrument kind.
func (me *metricExporter) Aggregation(ik metric.InstrumentKind) aggregation.Aggregation {
	return metric.DefaultAggregationSelector(ik)
}

// exportMetricDescriptor create MetricDescriptor from the record
// if the descriptor is not registered in Cloud Monitoring yet.
func (me *metricExporter) exportMetricDescriptor(ctx context.Context, rm metricdata.ResourceMetrics) error {
	if me.o.disableCreateMetricDescriptors {
		return nil
	}

	me.mdLock.Lock()
	defer me.mdLock.Unlock()
	mds := make(map[key]*googlemetricpb.MetricDescriptor)
	extraLabels := me.extraLabelsFromResource(rm.Resource)
	for _, scope := range rm.ScopeMetrics {
		for _, metrics := range scope.Metrics {
			k := keyOf(metrics, scope.Scope)

			if _, ok := me.mdCache[k]; ok {
				continue
			}

			if _, localok := mds[k]; !localok {
				md := me.recordToMdpb(metrics, extraLabels)
				mds[k] = md
			}
		}
	}

	// TODO: This process is synchronous and blocks longer time if records in cps
	// have many different descriptors. In the cps.ForEach above, it should spawn
	// goroutines to send CreateMetricDescriptorRequest asynchronously in the case
	// the descriptor does not exist in global cache (me.mdCache).
	// See details in #26.
	var err error
	for kmd, md := range mds {
		cmdErr := me.createMetricDescriptorIfNeeded(ctx, md)
		if cmdErr == nil {
			me.mdCache[kmd] = md
		}
		err = multierr.Append(err, cmdErr)
	}
	return err
}

func (me *metricExporter) createMetricDescriptorIfNeeded(ctx context.Context, md *googlemetricpb.MetricDescriptor) error {
	mdReq := &monitoringpb.GetMetricDescriptorRequest{
		Name: fmt.Sprintf("projects/%s/metricDescriptors/%s", me.o.projectID, md.Type),
	}
	_, err := me.client.GetMetricDescriptor(ctx, mdReq)
	if err == nil {
		// If the metric descriptor already exists, skip the CreateMetricDescriptor call.
		// Metric descriptors cannot be updated without deleting them first, so there
		// isn't anything we can do here:
		// https://cloud.google.com/monitoring/custom-metrics/creating-metrics#md-modify
		return nil
	}
	req := &monitoringpb.CreateMetricDescriptorRequest{
		Name:             fmt.Sprintf("projects/%s", me.o.projectID),
		MetricDescriptor: md,
	}
	_, err = me.client.CreateMetricDescriptor(ctx, req)
	return err
}

// exportTimeSeries create TimeSeries from the records in cps.
// res should be the common resource among all TimeSeries, such as instance id, application name and so on.
func (me *metricExporter) exportTimeSeries(ctx context.Context, rm metricdata.ResourceMetrics) error {
	tss, aggErr := me.recordsToTspbs(rm)
	if len(tss) == 0 {
		return aggErr
	}

	name := fmt.Sprintf("projects/%s", me.o.projectID)

	var createErrors []error
	for i := 0; i < len(tss); i += sendBatchSize {
		j := i + sendBatchSize
		if j >= len(tss) {
			j = len(tss)
		}

		// TODO: When this exporter is rewritten, support writing to multiple
		// projects based on the "gcp.project.id" resource.
		req := &monitoringpb.CreateTimeSeriesRequest{
			Name:       name,
			TimeSeries: tss[i:j],
		}

		err := me.client.CreateTimeSeries(ctx, req)
		if err != nil {
			createErrors = append(createErrors, err)
		}
	}

	return multierr.Append(aggErr, multierr.Combine(createErrors...))
}

func (me *metricExporter) extraLabelsFromResource(res *resource.Resource) *attribute.Set {
	set, _ := attribute.NewSetWithFiltered(res.Attributes(), me.o.resourceAttributeFilter)
	return &set
}

// descToMetricType converts descriptor to MetricType proto type.
// Basically this returns default value ("workload.googleapis.com/[metric type]").
func (me *metricExporter) descToMetricType(desc metricdata.Metrics) string {
	if formatter := me.o.metricDescriptorTypeFormatter; formatter != nil {
		return formatter(desc)
	}
	return fmt.Sprintf(cloudMonitoringMetricDescriptorNameFormat, desc.Name)
}

// metricTypeToDisplayName takes a GCM metric type, like (workload.googleapis.com/MyCoolMetric) and returns the display name.
func metricTypeToDisplayName(mURL string) string {
	// strip domain, keep path after domain.
	u, err := url.Parse(fmt.Sprintf("metrics://%s", mURL))
	if err != nil || u.Path == "" {
		return mURL
	}
	return strings.TrimLeft(u.Path, "/")
}

// recordToMdpb extracts data and converts them to googlemetricpb.MetricDescriptor.
func (me *metricExporter) recordToMdpb(metrics metricdata.Metrics, extraLabels *attribute.Set) *googlemetricpb.MetricDescriptor {
	name := metrics.Name
	typ := me.descToMetricType(metrics)
	kind, valueType := recordToMdpbKindType(metrics.Data)

	// Detailed explanations on MetricDescriptor proto is not documented on
	// generated Go packages. Refer to the original proto file.
	// https://github.com/googleapis/googleapis/blob/50af053/google/api/metric.proto#L33
	return &googlemetricpb.MetricDescriptor{
		Name:        name,
		DisplayName: metricTypeToDisplayName(typ),
		Type:        typ,
		MetricKind:  kind,
		ValueType:   valueType,
		Unit:        string(metrics.Unit),
		Description: metrics.Description,
		Labels:      labelDescriptors(metrics, extraLabels),
	}
}

func labelDescriptors(metrics metricdata.Metrics, extraLabels *attribute.Set) []*label.LabelDescriptor {
	labels := []*label.LabelDescriptor{}
	seenKeys := map[string]struct{}{}
	addAttributes := func(attr *attribute.Set) {
		iter := attr.Iter()
		for iter.Next() {
			kv := iter.Attribute()
			// Skip keys that have already been set
			if _, ok := seenKeys[normalizeLabelKey(string(kv.Key))]; ok {
				continue
			}
			labels = append(labels, &label.LabelDescriptor{
				Key: normalizeLabelKey(string(kv.Key)),
			})
			seenKeys[normalizeLabelKey(string(kv.Key))] = struct{}{}
		}
	}
	addAttributes(extraLabels)
	switch a := metrics.Data.(type) {
	case metricdata.Gauge[int64]:
		for _, pt := range a.DataPoints {
			addAttributes(&pt.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, pt := range a.DataPoints {
			addAttributes(&pt.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, pt := range a.DataPoints {
			addAttributes(&pt.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, pt := range a.DataPoints {
			addAttributes(&pt.Attributes)
		}
	case metricdata.Histogram:
		for _, pt := range a.DataPoints {
			addAttributes(&pt.Attributes)
		}
	}
	return labels
}

type attributes struct {
	attrs attribute.Set
}

func (attrs *attributes) GetString(key string) (string, bool) {
	value, ok := attrs.attrs.Value(attribute.Key(key))
	return value.AsString(), ok
}

// resourceToMonitoredResourcepb converts resource in OTel to MonitoredResource
// proto type for Cloud Monitoring.
//
// https://cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.monitoredResourceDescriptors
func (me *metricExporter) resourceToMonitoredResourcepb(res *resource.Resource) *monitoredrespb.MonitoredResource {
	gmr := resourcemapping.ResourceAttributesToMonitoredResource(&attributes{
		attrs: attribute.NewSet(res.Attributes()...),
	})
	newLabels := make(map[string]string, len(gmr.Labels))
	for k, v := range gmr.Labels {
		newLabels[k] = sanitizeUTF8(v)
	}
	mr := &monitoredrespb.MonitoredResource{
		Type:   gmr.Type,
		Labels: newLabels,
	}
	return mr
}

// recordToMdpbKindType return the mapping from OTel's record descriptor to
// Cloud Monitoring's MetricKind and ValueType.
func recordToMdpbKindType(a metricdata.Aggregation) (googlemetricpb.MetricDescriptor_MetricKind, googlemetricpb.MetricDescriptor_ValueType) {
	switch agg := a.(type) {
	case metricdata.Gauge[int64]:
		return googlemetricpb.MetricDescriptor_GAUGE, googlemetricpb.MetricDescriptor_INT64
	case metricdata.Gauge[float64]:
		return googlemetricpb.MetricDescriptor_GAUGE, googlemetricpb.MetricDescriptor_DOUBLE
	case metricdata.Sum[int64]:
		if agg.IsMonotonic {
			return googlemetricpb.MetricDescriptor_CUMULATIVE, googlemetricpb.MetricDescriptor_INT64
		}
		return googlemetricpb.MetricDescriptor_GAUGE, googlemetricpb.MetricDescriptor_INT64
	case metricdata.Sum[float64]:
		if agg.IsMonotonic {
			return googlemetricpb.MetricDescriptor_CUMULATIVE, googlemetricpb.MetricDescriptor_DOUBLE
		}
		return googlemetricpb.MetricDescriptor_GAUGE, googlemetricpb.MetricDescriptor_DOUBLE
	case metricdata.Histogram:
		return googlemetricpb.MetricDescriptor_CUMULATIVE, googlemetricpb.MetricDescriptor_DISTRIBUTION
	default:
		return googlemetricpb.MetricDescriptor_METRIC_KIND_UNSPECIFIED, googlemetricpb.MetricDescriptor_VALUE_TYPE_UNSPECIFIED
	}
}

// recordToMpb converts data from records to Metric proto type for Cloud Monitoring.
func (me *metricExporter) recordToMpb(metrics metricdata.Metrics, attributes attribute.Set, library instrumentation.Library, extraLabels *attribute.Set) *googlemetricpb.Metric {
	me.mdLock.RLock()
	defer me.mdLock.RUnlock()
	k := keyOf(metrics, library)
	md, ok := me.mdCache[k]
	if !ok {
		md = me.recordToMdpb(metrics, extraLabels)
	}

	labels := make(map[string]string)
	addAttributes := func(attr *attribute.Set) {
		iter := attr.Iter()
		for iter.Next() {
			kv := iter.Attribute()
			labels[normalizeLabelKey(string(kv.Key))] = sanitizeUTF8(kv.Value.Emit())
		}
	}
	addAttributes(extraLabels)
	addAttributes(&attributes)

	return &googlemetricpb.Metric{
		Type:   md.Type,
		Labels: labels,
	}
}

// recordToTspb converts record to TimeSeries proto type with common resource.
// ref. https://cloud.google.com/monitoring/api/ref_v3/rest/v3/TimeSeries
func (me *metricExporter) recordToTspb(m metricdata.Metrics, mr *monitoredrespb.MonitoredResource, library instrumentation.Scope, extraLabels *attribute.Set) ([]*monitoringpb.TimeSeries, error) {
	var tss []*monitoringpb.TimeSeries
	var aggErr error
	if m.Data == nil {
		return nil, nil
	}
	switch a := m.Data.(type) {
	case metricdata.Gauge[int64]:
		for _, point := range a.DataPoints {
			ts, err := gaugeToTimeSeries[int64](point, m, mr)
			if err != nil {
				aggErr = multierr.Append(aggErr, err)
				continue
			}
			ts.Metric = me.recordToMpb(m, point.Attributes, library, extraLabels)
			tss = append(tss, ts)
		}
	case metricdata.Gauge[float64]:
		for _, point := range a.DataPoints {
			ts, err := gaugeToTimeSeries[float64](point, m, mr)
			if err != nil {
				aggErr = multierr.Append(aggErr, err)
				continue
			}
			ts.Metric = me.recordToMpb(m, point.Attributes, library, extraLabels)
			tss = append(tss, ts)
		}
	case metricdata.Sum[int64]:
		for _, point := range a.DataPoints {
			var ts *monitoringpb.TimeSeries
			var err error
			if a.IsMonotonic {
				ts, err = sumToTimeSeries[int64](point, m, mr)
			} else {
				// Send non-monotonic sums as gauges
				ts, err = gaugeToTimeSeries[int64](point, m, mr)
			}
			if err != nil {
				aggErr = multierr.Append(aggErr, err)
				continue
			}
			ts.Metric = me.recordToMpb(m, point.Attributes, library, extraLabels)
			tss = append(tss, ts)
		}
	case metricdata.Sum[float64]:
		for _, point := range a.DataPoints {
			var ts *monitoringpb.TimeSeries
			var err error
			if a.IsMonotonic {
				ts, err = sumToTimeSeries[float64](point, m, mr)
			} else {
				// Send non-monotonic sums as gauges
				ts, err = gaugeToTimeSeries[float64](point, m, mr)
			}
			if err != nil {
				aggErr = multierr.Append(aggErr, err)
				continue
			}
			ts.Metric = me.recordToMpb(m, point.Attributes, library, extraLabels)
			tss = append(tss, ts)
		}
	case metricdata.Histogram:
		for _, point := range a.DataPoints {
			ts, err := histogramToTimeSeries(point, m, mr)
			if err != nil {
				aggErr = multierr.Append(aggErr, err)
				continue
			}
			ts.Metric = me.recordToMpb(m, point.Attributes, library, extraLabels)
			tss = append(tss, ts)
		}
	default:
		aggErr = multierr.Append(aggErr, errUnexpectedAggregationKind{kind: reflect.TypeOf(m.Data).String()})
	}
	return tss, aggErr
}

func (me *metricExporter) recordsToTspbs(rm metricdata.ResourceMetrics) ([]*monitoringpb.TimeSeries, error) {
	mr := me.resourceToMonitoredResourcepb(rm.Resource)
	extraLabels := me.extraLabelsFromResource(rm.Resource)

	var (
		tss    []*monitoringpb.TimeSeries
		errors []error
	)
	for _, scope := range rm.ScopeMetrics {
		for _, metrics := range scope.Metrics {
			ts, err := me.recordToTspb(metrics, mr, scope.Scope, extraLabels)
			if err != nil {
				errors = append(errors, err)
			}

			tss = append(tss, ts...)
		}
	}

	return tss, multierr.Combine(errors...)
}

func sanitizeUTF8(s string) string {
	return strings.ToValidUTF8(s, "�")
}

func gaugeToTimeSeries[N int64 | float64](point metricdata.DataPoint[N], metrics metricdata.Metrics, mr *monitoredrespb.MonitoredResource) (*monitoringpb.TimeSeries, error) {
	value, valueType := numberDataPointToValue(point)
	timestamp := timestamppb.New(point.Time)
	if err := timestamp.CheckValid(); err != nil {
		return nil, err
	}
	return &monitoringpb.TimeSeries{
		Resource:   mr,
		Unit:       string(metrics.Unit),
		MetricKind: googlemetricpb.MetricDescriptor_GAUGE,
		ValueType:  valueType,
		Points: []*monitoringpb.Point{{
			Interval: &monitoringpb.TimeInterval{
				EndTime: timestamp,
			},
			Value: value,
		}},
	}, nil
}

func sumToTimeSeries[N int64 | float64](point metricdata.DataPoint[N], metrics metricdata.Metrics, mr *monitoredrespb.MonitoredResource) (*monitoringpb.TimeSeries, error) {
	interval, err := toNonemptyTimeIntervalpb(point.StartTime, point.Time)
	if err != nil {
		return nil, err
	}
	value, valueType := numberDataPointToValue[N](point)
	return &monitoringpb.TimeSeries{
		Resource:   mr,
		Unit:       string(metrics.Unit),
		MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
		ValueType:  valueType,
		Points: []*monitoringpb.Point{{
			Interval: interval,
			Value:    value,
		}},
	}, nil
}

func histogramToTimeSeries(point metricdata.HistogramDataPoint, metrics metricdata.Metrics, mr *monitoredrespb.MonitoredResource) (*monitoringpb.TimeSeries, error) {
	interval, err := toNonemptyTimeIntervalpb(point.StartTime, point.Time)
	if err != nil {
		return nil, err
	}
	return &monitoringpb.TimeSeries{
		Resource:   mr,
		Unit:       string(metrics.Unit),
		MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
		ValueType:  googlemetricpb.MetricDescriptor_DISTRIBUTION,
		Points: []*monitoringpb.Point{{
			Interval: interval,
			Value:    histToTypedValue(point),
		}},
	}, nil
}

func toNonemptyTimeIntervalpb(start, end time.Time) (*monitoringpb.TimeInterval, error) {
	// The end time of a new interval must be at least a millisecond after the end time of the
	// previous interval, for all non-gauge types.
	// https://cloud.google.com/monitoring/api/ref_v3/rpc/google.monitoring.v3#timeinterval
	if end.Sub(start).Milliseconds() <= 1 {
		end = start.Add(time.Millisecond)
	}
	startpb := timestamppb.New(start)
	endpb := timestamppb.New(end)
	err := multierr.Combine(
		startpb.CheckValid(),
		endpb.CheckValid(),
	)
	if err != nil {
		return nil, err
	}

	return &monitoringpb.TimeInterval{
		StartTime: startpb,
		EndTime:   endpb,
	}, nil
}

func histToTypedValue(hist metricdata.HistogramDataPoint) *monitoringpb.TypedValue {
	counts := make([]int64, len(hist.BucketCounts))
	for i, v := range hist.BucketCounts {
		counts[i] = int64(v)
	}
	var mean float64
	if !math.IsNaN(hist.Sum) && hist.Count > 0 { // Avoid divide-by-zero
		mean = hist.Sum / float64(hist.Count)
	}
	return &monitoringpb.TypedValue{
		Value: &monitoringpb.TypedValue_DistributionValue{
			DistributionValue: &distribution.Distribution{
				Count:        int64(hist.Count),
				Mean:         mean,
				BucketCounts: counts,
				BucketOptions: &distribution.Distribution_BucketOptions{
					Options: &distribution.Distribution_BucketOptions_ExplicitBuckets{
						ExplicitBuckets: &distribution.Distribution_BucketOptions_Explicit{
							Bounds: hist.Bounds,
						},
					},
				},
				// TODO: support exemplars
			},
		},
	}
}

func numberDataPointToValue[N int64 | float64](
	point metricdata.DataPoint[N],
) (*monitoringpb.TypedValue, googlemetricpb.MetricDescriptor_ValueType) {
	switch v := any(point.Value).(type) {
	case int64:
		return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{
				Int64Value: v,
			}},
			googlemetricpb.MetricDescriptor_INT64
	case float64:
		return &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{
				DoubleValue: v,
			}},
			googlemetricpb.MetricDescriptor_DOUBLE
	}
	// It is impossible to reach this statement
	return nil, googlemetricpb.MetricDescriptor_INT64
}

// https://github.com/googleapis/googleapis/blob/c4c562f89acce603fb189679836712d08c7f8584/google/api/metric.proto#L149
//
// > The label key name must follow:
// >
// > * Only upper and lower-case letters, digits and underscores (_) are
// >   allowed.
// > * Label name must start with a letter or digit.
// > * The maximum length of a label name is 100 characters.
//
//	Note: this does not truncate if a label is too long.
func normalizeLabelKey(s string) string {
	if len(s) == 0 {
		return s
	}
	s = strings.Map(sanitizeRune, s)
	if unicode.IsDigit(rune(s[0])) {
		s = "key_" + s
	}
	return s
}

// converts anything that is not a letter or digit to an underscore.
func sanitizeRune(r rune) rune {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return r
	}
	// Everything else turns into an underscore
	return '_'
}
