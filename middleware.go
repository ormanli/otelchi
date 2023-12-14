package otelchi

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv1210 "go.opentelemetry.io/otel/semconv/v1.21.0"
	semconv140 "go.opentelemetry.io/otel/semconv/v1.4.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/riandyrn/otelchi"
)

// Middleware sets up a handler to start tracing the incoming
// requests. The serverName parameter should describe the name of the
// (virtual) server handling the request.
func Middleware(serverName string, opts ...Option) func(next http.Handler) http.Handler {
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	if cfg.MeterProvider == nil {
		cfg.MeterProvider = otel.GetMeterProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		tracerName,
		oteltrace.WithInstrumentationVersion(otelcontrib.Version()),
	)
	meter := cfg.MeterProvider.Meter(
		tracerName,
		otelmetric.WithInstrumentationVersion(otelcontrib.Version()),
	)
	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	requestBytesCounter, responseBytesCounter, serverLatencyMeasure := createMeasures(meter)
	return func(handler http.Handler) http.Handler {
		return traceware{
			serverName:           serverName,
			tracer:               tracer,
			propagators:          cfg.Propagators,
			handler:              handler,
			chiRoutes:            cfg.ChiRoutes,
			reqMethodInSpanName:  cfg.RequestMethodInSpanName,
			filter:               cfg.Filter,
			requestBytesCounter:  requestBytesCounter,
			responseBytesCounter: responseBytesCounter,
			serverLatencyMeasure: serverLatencyMeasure,
		}
	}
}

type traceware struct {
	serverName           string
	tracer               oteltrace.Tracer
	propagators          propagation.TextMapPropagator
	handler              http.Handler
	chiRoutes            chi.Routes
	reqMethodInSpanName  bool
	filter               func(r *http.Request) bool
	requestBytesCounter  otelmetric.Int64Counter
	responseBytesCounter otelmetric.Int64Counter
	serverLatencyMeasure otelmetric.Float64Histogram
}

type recordingResponseWriter struct {
	writer       http.ResponseWriter
	written      bool
	status       int
	startTime    time.Time
	writtenBytes int64
}

var rrwPool = &sync.Pool{
	New: func() interface{} {
		return &recordingResponseWriter{}
	},
}

func getRRW(writer http.ResponseWriter) *recordingResponseWriter {
	rrw := rrwPool.Get().(*recordingResponseWriter)
	rrw.written = false
	rrw.status = 0
	rrw.startTime = time.Now()
	rrw.writtenBytes = 0
	rrw.writer = httpsnoop.Wrap(writer, httpsnoop.Hooks{
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) {
				if !rrw.written {
					rrw.written = true
					rrw.status = http.StatusOK
				}
				n, err := next(b)
				rrw.writtenBytes += int64(n)
				return n, err
			}
		},
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(statusCode int) {
				if !rrw.written {
					rrw.written = true
					rrw.status = statusCode
				}
				next(statusCode)
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(r io.Reader) (int64, error) {
				if !rrw.written {
					rrw.written = true
					rrw.status = http.StatusOK
				}
				n, err := next(r)
				rrw.writtenBytes += n
				return n, err
			}
		},
	})
	return rrw
}

func putRRW(rrw *recordingResponseWriter) {
	rrw.writer = nil
	rrwPool.Put(rrw)
}

// ServeHTTP implements the http.Handler interface. It does the actual
// tracing of the request.
func (tw traceware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// skip if filter returns false
	if tw.filter != nil && !tw.filter(r) {
		tw.handler.ServeHTTP(w, r)
		return
	}

	// extract tracing header using propagator
	ctx := tw.propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	// create span, based on specification, we need to set already known attributes
	// when creating the span, the only thing missing here is HTTP route pattern since
	// in go-chi/chi route pattern could only be extracted once the request is executed
	// check here for details:
	//
	// https://github.com/go-chi/chi/issues/150#issuecomment-278850733
	//
	// if we have access to chi routes, we could extract the route pattern beforehand.
	spanName := ""
	routePattern := ""
	if tw.chiRoutes != nil {
		rctx := chi.NewRouteContext()
		if tw.chiRoutes.Match(rctx, r.Method, r.URL.Path) {
			routePattern = rctx.RoutePattern()
			spanName = addPrefixToSpanName(tw.reqMethodInSpanName, r.Method, routePattern)
		}
	}

	var metricAttributes []attribute.KeyValue

	ctx, span := tw.tracer.Start(
		ctx, spanName,
		oteltrace.WithAttributes(semconv140.NetAttributesFromHTTPRequest("tcp", r)...),
		oteltrace.WithAttributes(semconv140.EndUserAttributesFromHTTPRequest(r)...),
		oteltrace.WithAttributes(semconv140.HTTPServerAttributesFromHTTPRequest(tw.serverName, routePattern, r)...),
		oteltrace.WithSpanKind(oteltrace.SpanKindServer),
	)
	defer span.End()

	var rbc readerBytesCounter
	// if request body is nil or NoBody, we don't want to mutate the body as it
	// will affect the identity of it in an unforeseeable way because we assert
	// ReadCloser fulfills a certain interface, and it is indeed nil or NoBody.
	if r.Body != nil && r.Body != http.NoBody {
		rbc.ReadCloser = r.Body
		r.Body = &rbc
	}

	// get recording response writer
	rrw := getRRW(w)
	defer putRRW(rrw)

	// execute next http handler
	r = r.WithContext(ctx)
	tw.handler.ServeHTTP(rrw.writer, r)

	// set span name & http route attribute if necessary
	if len(routePattern) == 0 {
		routePattern = chi.RouteContext(r.Context()).RoutePattern()
		span.SetAttributes(semconv1210.HTTPRouteKey.String(routePattern))
		metricAttributes = append(metricAttributes, semconv1210.HTTPRoute(routePattern))

		spanName = addPrefixToSpanName(tw.reqMethodInSpanName, r.Method, routePattern)
		span.SetName(spanName)
	}

	if r.Method != "" {
		metricAttributes = append(metricAttributes, semconv1210.HTTPMethodKey.String(r.Method))
	} else {
		metricAttributes = append(metricAttributes, semconv1210.HTTPMethodKey.String(http.MethodGet))
	}

	// set status code attribute
	span.SetAttributes(semconv1210.HTTPStatusCodeKey.Int(rrw.status))
	metricAttributes = append(metricAttributes, semconv1210.HTTPStatusCode(rrw.status))

	// set span status
	spanStatus, spanMessage := semconv140.SpanStatusFromHTTPStatusCode(rrw.status)
	span.SetStatus(spanStatus, spanMessage)

	o := metric.WithAttributes(metricAttributes...)
	tw.requestBytesCounter.Add(ctx, rbc.bytes(), o)
	tw.responseBytesCounter.Add(ctx, rrw.writtenBytes, o)

	// Use floating point division here for higher precision (instead of Millisecond method).
	elapsedTime := float64(time.Since(rrw.startTime)) / float64(time.Millisecond)

	tw.serverLatencyMeasure.Record(ctx, elapsedTime, o)
}

func addPrefixToSpanName(shouldAdd bool, prefix, spanName string) string {
	// in chi v5.0.8, the root route will be returned has an empty string
	// (see github.com/go-chi/chi/v5@v5.0.8/context.go:126)
	if spanName == "" {
		spanName = "/"
	}

	if shouldAdd && len(spanName) > 0 {
		spanName = prefix + " " + spanName
	}
	return spanName
}

func handleErr(err error) {
	if err != nil {
		otel.Handle(err)
	}
}

func createMeasures(m otelmetric.Meter) (otelmetric.Int64Counter, otelmetric.Int64Counter, otelmetric.Float64Histogram) {
	var err error
	requestBytesCounter, err := m.Int64Counter(
		RequestContentLength,
		metric.WithUnit("By"),
		metric.WithDescription("Measures the size of HTTP request content length (uncompressed)"),
	)
	handleErr(err)

	responseBytesCounter, err := m.Int64Counter(
		ResponseContentLength,
		metric.WithUnit("By"),
		metric.WithDescription("Measures the size of HTTP response content length (uncompressed)"),
	)
	handleErr(err)

	serverLatencyMeasure, err := m.Float64Histogram(
		ServerLatency,
		metric.WithUnit("ms"),
		metric.WithDescription("Measures the duration of HTTP request handling"),
	)
	handleErr(err)

	return requestBytesCounter, responseBytesCounter, serverLatencyMeasure
}

const (
	RequestContentLength  = "http.server.request_content_length"  // Incoming request bytes total
	ResponseContentLength = "http.server.response_content_length" // Incoming response bytes total
	ServerLatency         = "http.server.duration"                // Incoming end to end duration, milliseconds
)
