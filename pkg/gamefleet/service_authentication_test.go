package gamefleet

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func TestExpiredServiceCredentialFailsOnceWithoutBlamingPlayerSession(t *testing.T) {
	var calls atomic.Int32
	server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		serviceBackendTestError(w, http.StatusUnauthorized)
	}))
	defer server.Close()
	search, history := serviceBackendClients(t, server.URL, server.URL)
	backend, err := newServiceBackend(search, history)
	if err != nil {
		t.Fatal(err)
	}
	b := bridge{backend: backend, config: Config{ApplicationID: search.config.ApplicationID,
		Region: search.config.Region, Compatibility: search.config.Compatibility}}
	_, err = b.rpc(bridgeCtx(serviceBackendTestUser), CurrentRPC, serviceBackendCurrentPayload())
	var rpcErr *runtime.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != 9 || rpcErr.Message != "gamefleet_service_authentication_unavailable" {
		t.Fatalf("machine credential failure must be a terminal service condition, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("permanent service authentication retried %d times", calls.Load())
	}
}

func TestServiceAuthenticationClassificationPreservesOtherUpstreamFailures(t *testing.T) {
	for _, from := range []func(int) error{
		func(status int) error { return &ServiceHistoryError{Status: status, Code: "private-code"} },
		func(status int) error { return &ServiceSearchError{Status: status, Code: "private-code"} },
	} {
		for _, status := range []int{401, 403, 404, 409, 422, 429, 503} {
			t.Run(fmt.Sprint(status), func(t *testing.T) {
				err := playerError(servicePlayerError(from(status)))
				var rpcErr *runtime.Error
				if !errors.As(err, &rpcErr) {
					t.Fatal("expected sanitized RPC error")
				}
				if status == 401 {
					if rpcErr.Code != 9 || rpcErr.Message != "gamefleet_service_authentication_unavailable" {
						t.Fatal("service credential failure misclassified")
					}
				} else if rpcErr.Message == "gamefleet_service_authentication_unavailable" || rpcErr.Code == 16 {
					t.Fatal("different upstream failure became a service or player authentication error")
				}
			})
		}
	}
}
