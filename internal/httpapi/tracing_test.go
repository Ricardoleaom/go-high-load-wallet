package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
)

func TestHandlerStartsAServerSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	metrics := observability.New()
	issuer := jwttest.NewIssuer(t)
	auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{JWKSURL: issuer.JWKSURL(), RefreshInterval: time.Hour, Issuer: issuer.URL(), Audience: jwttest.Audience}, metrics)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	handler := httpapi.NewHandler(httpapi.Dependencies{Auth: auth, Metrics: metrics})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/live", nil))

	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer {
			return
		}
	}
	t.Fatalf("no server span recorded; %d spans ended", len(recorder.Ended()))
}

func TestPublicHandlerNoLongerServesMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	metrics := observability.New()
	issuer := jwttest.NewIssuer(t)
	auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{JWKSURL: issuer.JWKSURL(), RefreshInterval: time.Hour, Issuer: issuer.URL(), Audience: jwttest.Audience}, metrics)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	handler := httpapi.NewHandler(httpapi.Dependencies{Auth: auth, Metrics: metrics})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code == http.StatusOK {
		t.Fatal("the public port still serves /metrics")
	}
}
