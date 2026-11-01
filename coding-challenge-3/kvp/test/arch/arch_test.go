// Package arch enforces the Clean Architecture dependency rule with `go list`:
// dependencies point inward (adapter → usecase → domain), and the inner
// layers never reach infrastructure (database, broker, Redis, HTTP).
// Runs with the unit tests (`make test`).
package arch

import (
	"os/exec"
	"strings"
	"testing"
)

var services = []string{"ride", "driver", "dispatch"}

// deps returns every package (transitively) imported by pkg.
func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

// imports returns the direct imports of pkg.
func imports(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", `{{join .Imports " "}}`, pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

// Infrastructure that inner layers must never depend on, even transitively.
var infrastructure = []string{
	"github.com/jackc/pgx",         // Postgres driver
	"github.com/twmb/franz-go",     // Kafka/Redpanda client
	"github.com/redis/go-redis",    // Redis client
	"go.opentelemetry.io/otel/sdk", // telemetry SDK (the API is allowed)
	"go.opentelemetry.io/otel/exporters",
	"ridehail/pkg/broker", // outbox/inbox/consumer infrastructure
	"ridehail/pkg/pgutil",
	"ridehail/pkg/svc",
	"ridehail/pkg/httpx",
	"ridehail/pkg/grpcx",
	"google.golang.org/grpc", // gRPC transport belongs in adapter/grpc
	"ridehail/api/gen",       // generated gRPC/protobuf code
}

func TestDomainIsPure(t *testing.T) {
	for _, s := range services {
		pkg := "ridehail/services/" + s + "/internal/domain"
		for _, imp := range imports(t, pkg) {
			if strings.HasPrefix(imp, "ridehail/") && imp != "ridehail/pkg/geo" {
				t.Errorf("%s imports %s: the domain may only use the standard library and pkg/geo", pkg, imp)
			}
			if strings.Contains(imp, ".") && !strings.HasPrefix(imp, "ridehail/") { // third-party
				t.Errorf("%s imports third-party %s", pkg, imp)
			}
		}
	}
}

func TestInnerLayersDoNotReachInfrastructure(t *testing.T) {
	for _, s := range services {
		for _, layer := range []string{"domain", "usecase"} {
			pkg := "ridehail/services/" + s + "/internal/" + layer
			for _, d := range deps(t, pkg) {
				for _, bad := range infrastructure {
					if d == bad || strings.HasPrefix(d, bad+"/") {
						t.Errorf("%s depends on %s (infrastructure belongs in adapters)", pkg, d)
					}
				}
				if strings.Contains(d, "/internal/adapter") {
					t.Errorf("%s depends on adapter %s (dependencies must point inward)", pkg, d)
				}
				// None of OUR packages reachable from an inner layer may speak HTTP.
				// (The OpenTelemetry propagation API imports net/http for its header
				// carrier; that is a cross-cutting API we accept.)
				if strings.HasPrefix(d, "ridehail/") {
					for _, imp := range imports(t, d) {
						if imp == "net/http" {
							t.Errorf("%s (reachable from %s) imports net/http", d, pkg)
						}
					}
				}
			}
		}
	}
}

func TestMessagingContractIsPure(t *testing.T) {
	for _, d := range deps(t, "ridehail/pkg/messaging") {
		for _, bad := range infrastructure {
			if d == bad || strings.HasPrefix(d, bad+"/") {
				t.Errorf("pkg/messaging (the message contract) depends on %s", d)
			}
		}
	}
}

func TestAdaptersDoNotDependOnEachOther(t *testing.T) {
	for _, s := range services {
		base := "ridehail/services/" + s + "/internal/adapter/"
		out, err := exec.Command("go", "list", base+"...").Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			for _, imp := range imports(t, pkg) {
				if strings.HasPrefix(imp, base) {
					t.Errorf("%s imports sibling adapter %s (adapters talk through use cases)", pkg, imp)
				}
			}
		}
	}
}
