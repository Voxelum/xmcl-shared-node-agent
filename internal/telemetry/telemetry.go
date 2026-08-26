package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/voxelum/xmcl-shared-node-agent/internal/otlpconfig"
)

type Provider struct {
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	transport      http.RoundTripper
}

type WorkspaceMetrics struct {
	LogicalBytes      int64
	ActualObjectBytes int64
	RestoreBytes      int64
	SyncBytes         int64
	RestoreFailures   int64
	SyncFailures      int64
}

func New(
	ctx context.Context,
	endpoint string,
	headers map[string]string,
	nodeID, region, version string,
) (*Provider, error) {
	if endpoint == "" {
		return &Provider{transport: http.DefaultTransport}, nil
	}
	if err := otlpconfig.ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Host == "" {
		return nil, errors.New("invalid OTLP endpoint")
	}
	basePath := strings.TrimSuffix(parsedEndpoint.Path, "/")
	traceOptions := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(parsedEndpoint.Host),
		otlptracehttp.WithURLPath(basePath + "/v1/traces"),
		otlptracehttp.WithHeaders(headers),
	}
	metricOptions := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(parsedEndpoint.Host),
		otlpmetrichttp.WithURLPath(basePath + "/v1/metrics"),
		otlpmetrichttp.WithHeaders(headers),
	}
	if parsedEndpoint.Scheme == "http" {
		traceOptions = append(traceOptions, otlptracehttp.WithInsecure())
		metricOptions = append(metricOptions, otlpmetrichttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(
		ctx,
		traceOptions...,
	)
	if err != nil {
		return nil, err
	}
	metricExporter, err := otlpmetrichttp.New(
		ctx,
		metricOptions...,
	)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("xmcl-shared-node-agent"),
		semconv.ServiceVersion(version),
		semconv.HostID(nodeID),
		semconv.CloudRegion(region),
		attribute.String("cloud.provider", "lightnode"),
		attribute.String("xmcl.node.id", nodeID),
	))
	if err != nil {
		return nil, err
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(
			metricExporter,
			sdkmetric.WithInterval(30*time.Second),
		)),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return &Provider{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		transport: otelhttp.NewTransport(
			http.DefaultTransport,
			otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
				return request.Method + " " + request.URL.Host
			}),
		),
	}, nil
}

func (p *Provider) RegisterWorkspaceMetrics(snapshot func() WorkspaceMetrics) error {
	if p.meterProvider == nil {
		return nil
	}
	meter := p.meterProvider.Meter("github.com/voxelum/xmcl-shared-node-agent/internal/workspace")
	logicalBytes, err := meter.Int64ObservableGauge("xmcl.shared.workspace.logical_bytes")
	if err != nil {
		return err
	}
	actualBytes, err := meter.Int64ObservableGauge("xmcl.shared.workspace.actual_object_bytes")
	if err != nil {
		return err
	}
	restoreBytes, err := meter.Int64ObservableCounter("xmcl.shared.workspace.restore_download_bytes")
	if err != nil {
		return err
	}
	syncBytes, err := meter.Int64ObservableCounter("xmcl.shared.workspace.sync_upload_bytes")
	if err != nil {
		return err
	}
	restoreFailures, err := meter.Int64ObservableCounter("xmcl.shared.workspace.restore_failures")
	if err != nil {
		return err
	}
	syncFailures, err := meter.Int64ObservableCounter("xmcl.shared.workspace.sync_failures")
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		value := snapshot()
		observer.ObserveInt64(logicalBytes, value.LogicalBytes)
		observer.ObserveInt64(actualBytes, value.ActualObjectBytes)
		observer.ObserveInt64(restoreBytes, value.RestoreBytes)
		observer.ObserveInt64(syncBytes, value.SyncBytes)
		observer.ObserveInt64(restoreFailures, value.RestoreFailures)
		observer.ObserveInt64(syncFailures, value.SyncFailures)
		return nil
	}, logicalBytes, actualBytes, restoreBytes, syncBytes, restoreFailures, syncFailures)
	return err
}

func (p *Provider) HTTPClient(timeoutClient *http.Client) *http.Client {
	client := *timeoutClient
	client.Transport = p.transport
	return &client
}

func (p *Provider) Shutdown(ctx context.Context) error {
	var shutdownErrors []error
	if p.tracerProvider != nil {
		shutdownErrors = append(shutdownErrors, p.tracerProvider.Shutdown(ctx))
	}
	if p.meterProvider != nil {
		shutdownErrors = append(shutdownErrors, p.meterProvider.Shutdown(ctx))
	}
	return errors.Join(shutdownErrors...)
}
