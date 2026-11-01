//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// contract validates real gateway responses against api/openapi.yaml, so the
// spec cannot drift from the implementation.
type contract struct {
	t      *testing.T
	router routers.Router
}

func newContract(t *testing.T) *contract {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("invalid spec: %v", err)
	}
	doc.Servers = openapi3.Servers{{URL: gateway}}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	openapi3filter.RegisterBodyDecoder("application/problem+json", openapi3filter.JSONBodyDecoder)
	return &contract{t: t, router: router}
}

// call sends a request and fails the test if the status is not want or the
// response does not match the spec. It returns the decoded JSON body.
func (c *contract) call(want int, method, path string, body any, headers ...string) map[string]any {
	c.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, gateway+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, respBody)
	}

	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Fatalf("%s %s is not in the spec: %v", method, path, err)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Body:                   io.NopCloser(bytes.NewReader(respBody)),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		c.t.Fatalf("%s %s: response does not match api/openapi.yaml: %v\nbody: %s", method, path, err, respBody)
	}
	out := map[string]any{}
	_ = json.Unmarshal(respBody, &out)
	return out
}

// Every REST endpoint and its main error responses, checked against the spec.
func TestRESTResponsesMatchOpenAPI(t *testing.T) {
	c := newContract(t)
	a := newArea()
	driver := newDriver(t, a, 100)
	other := newDriver(t, a, 4000) // outside the radius: never offered
	key := "e2e-openapi-" + driver
	ride := map[string]any{"passengerId": "P001", "pickup": a.center, "destination": a.at(5000)}

	// Rides
	created := c.call(201, "POST", "/rides", ride, "Idempotency-Key", key)
	id := created["rideId"].(string)
	c.call(201, "POST", "/rides", ride, "Idempotency-Key", key) // replay
	c.call(422, "POST", "/rides", map[string]any{"passengerId": "P002", "pickup": a.center, "destination": a.at(5000)}, "Idempotency-Key", key)
	c.call(400, "POST", "/rides", ride)
	c.call(422, "POST", "/rides", map[string]any{"passengerId": "P001", "pickup": point{91, 0}, "destination": a.at(5000)}, "Idempotency-Key", key+"-bad")
	c.call(400, "POST", "/rides", map[string]any{"unknown": true}, "Idempotency-Key", key+"-json")
	c.call(404, "GET", "/rides/00000000-0000-7000-8000-000000000000", nil)

	// Offers
	eventually(t, 10*time.Second, rideIs(t, id, "DRIVER_ASSIGNED", driver))
	c.call(200, "GET", "/rides/"+id, nil)
	c.call(200, "GET", "/drivers/"+driver+"/offer", nil)
	c.call(204, "GET", "/drivers/"+other+"/offer", nil)
	c.call(404, "POST", "/rides/"+id+"/offers/"+other+"/accept", nil)
	c.call(200, "POST", "/rides/"+id+"/offers/"+driver+"/accept", nil)
	c.call(200, "POST", "/rides/"+id+"/offers/"+driver+"/accept", nil) // replay
	c.call(409, "POST", "/rides/"+id+"/offers/"+driver+"/reject", nil)
	c.call(200, "GET", "/rides/"+id+"/dispatch", nil)
	c.call(404, "GET", "/rides/00000000-0000-7000-8000-000000000000/dispatch", nil)

	// Drivers
	c.call(200, "GET", "/drivers/"+driver, nil)
	c.call(404, "GET", "/drivers/E2E-NOPE", nil)
	c.call(409, "PUT", "/drivers/"+driver+"/availability", map[string]any{"status": "OFFLINE"}) // BUSY
	c.call(422, "PUT", "/drivers/"+other+"/availability", map[string]any{"status": "BUSY"})
	c.call(200, "PUT", "/drivers/"+other+"/availability", map[string]any{"status": "OFFLINE"})
	c.call(200, "PUT", "/drivers/"+other+"/location", a.at(4100))
	c.call(422, "PUT", "/drivers/"+other+"/location", point{95, 0})

	// Ride lifecycle
	eventually(t, 10*time.Second, rideIs(t, id, "DRIVER_ACCEPTED", driver))
	c.call(200, "POST", "/rides/"+id+"/complete", nil)
	c.call(409, "POST", "/rides/"+id+"/cancel", nil)
	c.call(200, "GET", "/rides/"+id+"/history", nil)
}
