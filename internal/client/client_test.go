package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmit(t *testing.T) {
	var auth, contentType string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_1"}`))
	}))
	defer srv.Close()

	c := &Client{Gateway: srv.URL, Token: "herald_axis_secret"}
	res, err := c.Submit(context.Background(), Event{
		Project: "axis", Status: "success", Title: "t", Agent: "a", Host: "h", OccurrenceID: "occ-1", IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "evt_1" {
		t.Fatalf("id = %q", res.ID)
	}
	if res.Deliveries != DeliveriesUnknown {
		t.Fatalf("deliveries = %d, want unknown when the gateway omits it", res.Deliveries)
	}
	if auth != "Bearer herald_axis_secret" || contentType != "application/json" {
		t.Fatalf("auth=%q ct=%q", auth, contentType)
	}
	if ctx, _ := body["context"].(map[string]any); ctx["host"] != "h" {
		t.Fatalf("body = %v", body)
	}
	if body["occurrenceId"] != "occ-1" || body["idempotencyKey"] != "k" {
		t.Fatalf("body = %v", body)
	}
}

func TestSubmitReportsDeliveryCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_1","deliveries":0}`))
	}))
	defer srv.Close()

	res, err := (&Client{Gateway: srv.URL}).Submit(context.Background(), Event{Status: "success", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "evt_1" || res.Deliveries != 0 {
		t.Fatalf("res = %+v, want id evt_1 and 0 deliveries", res)
	}
}

func TestSubmitGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"title is required","field":"title"}}`))
	}))
	defer srv.Close()

	_, err := (&Client{Gateway: srv.URL}).Submit(context.Background(), Event{Status: "success", Title: ""})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v", err)
	}
	if httpErr.StatusCode != http.StatusUnprocessableEntity || httpErr.Code != "validation_failed" || httpErr.Field != "title" {
		t.Fatalf("httpErr = %+v", httpErr)
	}
	if httpErr.Error() == "" {
		t.Fatal("empty error message")
	}
}

func TestSubmitNoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := (&Client{Gateway: srv.URL}).Submit(context.Background(), Event{Status: "success", Title: "t"}); err == nil {
		t.Fatal("expected error for missing id")
	}
}

func TestSubmitTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	if _, err := (&Client{Gateway: srv.URL}).Submit(context.Background(), Event{Status: "success", Title: "t"}); err == nil {
		t.Fatal("expected transport error")
	}
}
