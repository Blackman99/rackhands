package redfish

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionAuthAndErrors(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/redfish/v1/SessionService/Sessions":
			w.Header().Set("X-Auth-Token", "tok")
			w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/1")
			w.WriteHeader(201)
		case r.Method == "DELETE":
			deleted = r.Header.Get("X-Auth-Token") == "tok"
			w.WriteHeader(204)
		case r.Header.Get("X-Auth-Token") != "tok":
			w.WriteHeader(401)
		case r.URL.Path == "/redfish/v1/Bad":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"generic","@Message.ExtendedInfo":[{"Message":"The value 'Foo' for ResetType is not allowed."}]}}`))
		default:
			_, _ = w.Write([]byte(`{"@odata.id":"/redfish/v1/","Name":"root","Status":{"Health":"OK"}}`))
		}
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, Username: "u", Password: "p", Session: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o, err := c.Get(ctx, "/redfish/v1/")
	if err != nil || o.ID() != "/redfish/v1/" || o.Str("Status.Health") != "OK" {
		t.Fatalf("%v %v", err, o)
	}
	_, err = c.Get(ctx, "/redfish/v1/Bad")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 || he.Message != "The value 'Foo' for ResetType is not allowed." {
		t.Fatalf("extended info not surfaced: %v", err)
	}
	c.Close(ctx)
	if !deleted {
		t.Fatal("session not deleted on Close")
	}
}

func TestObjectDottedKeys(t *testing.T) {
	o := Object{"@odata.id": "/x", "Boot": map[string]any{"BootSourceOverrideTarget@Redfish.AllowableValues": []any{"Pxe"}},
		"Oem": map[string]any{"Hpe": map[string]any{"AggregateHealthStatus": map[string]any{"Fans": map[string]any{"Status": map[string]any{"Health": "Warning"}}}}}}
	if o.ID() != "/x" || len(o.Strings("Boot.BootSourceOverrideTarget@Redfish.AllowableValues")) != 1 ||
		o.Str("Oem.Hpe.AggregateHealthStatus.Fans.Status.Health") != "Warning" || o.Str("Missing.Key") != "" {
		t.Fatal("accessors")
	}
}

func TestBadAddress(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://"}); err == nil {
		t.Fatal("empty host accepted")
	}
	c, _ := New(Options{BaseURL: "10.1.2.3:8443"})
	if c.Host() != "10.1.2.3:8443" {
		t.Fatal(c.Host())
	}
}
