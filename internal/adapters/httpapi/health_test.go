package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthLive(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	response := httptest.NewRecorder()

	healthLive(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d",
			http.StatusOK, response.Code)
	}

	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("unexpected content type: %s", contentType)
	}
}

func TestHealthLiveRejectsNonGet(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/health/live", nil)
	response := httptest.NewRecorder()

	healthLive(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d",
			http.StatusMethodNotAllowed, response.Code)
	}
}
