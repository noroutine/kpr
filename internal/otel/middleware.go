package otel

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTPMiddleware wraps an HTTP handler with OpenTelemetry instrumentation
// If OTEL is disabled, it returns the original handler unchanged
func HTTPMiddleware(handler http.Handler, serviceName string, enabled bool) http.Handler {
	if !enabled {
		return handler
	}

	return otelhttp.NewHandler(handler, serviceName,
		otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			return operation + " " + r.Method + " " + r.URL.Path
		}),
	)
}
