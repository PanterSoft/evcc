package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evcc-io/evcc/server/service"
	"github.com/stretchr/testify/assert"
)

func TestIaqualinkDevices_MissingCredentials(t *testing.T) {
	req := httptest.NewRequest("GET", "/devices?user=foo", nil)
	w := httptest.NewRecorder()

	iaqualinkDevices(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestIaqualinkDevices_Routing verifies the handler is reachable under the path
// the template's `service:` reference resolves to.
func TestIaqualinkDevices_Routing(t *testing.T) {
	req := httptest.NewRequest("GET", "/iaqualink/devices?user=foo", nil)
	w := httptest.NewRecorder()

	service.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
