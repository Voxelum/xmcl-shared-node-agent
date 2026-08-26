package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderExportsInstrumentedHTTPTrace(t *testing.T) {
	var traceExports atomic.Int64
	var metricExports atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "node-scoped-value" {
			t.Errorf("missing node-scoped authorization header")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if len(body) == 0 {
			t.Error("empty telemetry export")
		}
		switch request.URL.Path {
		case "/v1/traces":
			traceExports.Add(1)
		case "/v1/metrics":
			metricExports.Add(1)
		default:
			t.Errorf("export path = %q", request.URL.Path)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	provider, err := New(
		t.Context(),
		collector.URL,
		map[string]string{"Authorization": "node-scoped-value"},
		"node-1",
		"mow",
		"test-version",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.RegisterWorkspaceMetrics(func() WorkspaceMetrics {
		return WorkspaceMetrics{LogicalBytes: 42, RestoreBytes: 21}
	}); err != nil {
		t.Fatal(err)
	}
	client := provider.HTTPClient(&http.Client{Timeout: time.Second})
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	shutdownContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := provider.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	if traceExports.Load() == 0 {
		t.Fatal("instrumented request did not export a trace")
	}
	if metricExports.Load() == 0 {
		t.Fatal("workspace snapshot did not export metrics")
	}
}

func TestDisabledProviderUsesDefaultTransport(t *testing.T) {
	provider, err := New(t.Context(), "", nil, "node-1", "mow", "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if provider.transport != http.DefaultTransport {
		t.Fatal("disabled provider replaced the default transport")
	}
	provider.RecordCommandFailure(t.Context(), "workspace.restore_and_start")
	if err := provider.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}
