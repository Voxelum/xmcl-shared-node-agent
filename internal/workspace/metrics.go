package workspace

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type StorageMetricsSnapshot struct {
	LogicalBytes      int64
	ActualObjectBytes int64
	RestoreBytes      int64
	SyncBytes         int64
	RestoreFailures   int64
	SyncFailures      int64
}

type storageMetrics struct {
	logicalBytes      atomic.Int64
	actualObjectBytes atomic.Int64
	restoreBytes      atomic.Int64
	syncBytes         atomic.Int64
	restoreFailures   atomic.Int64
	syncFailures      atomic.Int64
}

func (m *storageMetrics) snapshot() StorageMetricsSnapshot {
	return StorageMetricsSnapshot{
		LogicalBytes:      m.logicalBytes.Load(),
		ActualObjectBytes: m.actualObjectBytes.Load(),
		RestoreBytes:      m.restoreBytes.Load(),
		SyncBytes:         m.syncBytes.Load(),
		RestoreFailures:   m.restoreFailures.Load(),
		SyncFailures:      m.syncFailures.Load(),
	}
}

func (m *Manager) Metrics() StorageMetricsSnapshot {
	return m.metrics.snapshot()
}

// MetricsHandler intentionally has no authentication because systemd binds it
// only to loopback. It never includes object names, workspace data, or secrets.
func (m *Manager) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		metrics := m.Metrics()
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprintf(writer, "# TYPE xmcl_shared_workspace_logical_bytes gauge\nxmcl_shared_workspace_logical_bytes %d\n", metrics.LogicalBytes)
		_, _ = fmt.Fprintf(writer, "# TYPE xmcl_shared_workspace_object_bytes gauge\nxmcl_shared_workspace_object_bytes %d\n", metrics.ActualObjectBytes)
		_, _ = fmt.Fprintf(writer, "# TYPE xmcl_shared_workspace_restore_download_bytes_total counter\nxmcl_shared_workspace_restore_download_bytes_total %d\n", metrics.RestoreBytes)
		_, _ = fmt.Fprintf(writer, "# TYPE xmcl_shared_workspace_sync_upload_bytes_total counter\nxmcl_shared_workspace_sync_upload_bytes_total %d\n", metrics.SyncBytes)
		_, _ = fmt.Fprintf(writer, "# TYPE xmcl_shared_workspace_restore_failures_total counter\nxmcl_shared_workspace_restore_failures_total %d\n", metrics.RestoreFailures)
		_, _ = fmt.Fprintf(writer, "# TYPE xmcl_shared_workspace_sync_failures_total counter\nxmcl_shared_workspace_sync_failures_total %d\n", metrics.SyncFailures)
	})
}
