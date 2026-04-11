package charger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tekkamanendless/iaqualink"
)

func TestIAquaLinkFromConfig(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		errMsg string
	}{
		{
			name: "valid local mode config (no device: compatibility probe fails)",
			config: map[string]any{
				"uri": "http://127.0.0.1:1",
			},
			errMsg: "no compatible IAquaLink device at",
		},
		{
			name: "valid cloud mode config fails without network or real API",
			config: map[string]any{
				"email":    "test@example.com",
				"password": "password123",
				"device":   "device123",
			},
			errMsg: "IAquaLink login failed",
		},
		{
			name: "cloud mode with user alias fails login",
			config: map[string]any{
				"user":     "test@example.com",
				"password": "password123",
				"device":   "device123",
			},
			errMsg: "IAquaLink login failed",
		},
		{
			name: "missing both uri and credentials",
			config: map[string]any{
				"device": "device123",
			},
			errMsg: "must provide either uri (local mode) or user/email and password (cloud mode)",
		},
		{
			name: "missing email in cloud mode",
			config: map[string]any{
				"password": "password123",
				"device":   "device123",
			},
			errMsg: "must provide either uri (local mode) or user/email and password (cloud mode)",
		},
		{
			name: "missing password in cloud mode",
			config: map[string]any{
				"email":  "test@example.com",
				"device": "device123",
			},
			errMsg: "must provide either uri (local mode) or user/email and password (cloud mode)",
		},
		{
			name: "missing device in cloud mode",
			config: map[string]any{
				"email":    "test@example.com",
				"password": "password123",
			},
			errMsg: "device is required for cloud mode (serial number or name in IAquaLink)",
		},
		{
			name: "both uri and credentials provided",
			config: map[string]any{
				"uri":      "http://192.168.1.100",
				"email":    "test@example.com",
				"password": "password123",
				"device":   "device123",
			},
			errMsg: "cannot use both uri (local) and email/password (cloud) - choose one mode",
		},
		{
			name: "uri with email only",
			config: map[string]any{
				"uri":   "http://192.168.1.100",
				"email": "test@example.com",
			},
			errMsg: "cannot use both uri (local) and email/password (cloud) - choose one mode",
		},
		{
			name: "uri with password only",
			config: map[string]any{
				"uri":      "http://192.168.1.100",
				"password": "password123",
			},
			errMsg: "cannot use both uri (local) and email/password (cloud) - choose one mode",
		},
		{
			name: "invalid local uri",
			config: map[string]any{
				"uri": "://bad",
			},
			errMsg: "invalid uri for local mode",
		},
		{
			name: "skipverify and allowunsupported rejected",
			config: map[string]any{
				"uri":              "http://192.168.1.1",
				"skipverify":       true,
				"allowunsupported": true,
			},
			errMsg: "skipverify and allowunsupported cannot be combined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			charger, err := NewIAquaLinkFromConfig(ctx, tt.config)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
			assert.Nil(t, charger)
		})
	}
}

func TestIAquaLink_InterfaceImplementation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"mode":"smart"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	config := map[string]any{
		"uri": srv.URL,
	}

	charger, err := NewIAquaLinkFromConfig(ctx, config)
	require.NoError(t, err)
	require.NotNil(t, charger)

	_, ok := charger.(api.Charger)
	assert.True(t, ok, "IAquaLink should implement api.Charger")

	_, ok = charger.(api.ChargerEx)
	assert.True(t, ok, "IAquaLink should implement api.ChargerEx via SgReady")

	_, ok = charger.(api.Dimmer)
	assert.True(t, ok, "IAquaLink should implement api.Dimmer via SgReady")
}

func TestParseModeFromResponse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"boost keyword", `{"run":"boost"}`, 3},
		{"smart keyword", `state: smart`, 2},
		{"normal keyword", `normal`, 2},
		{"eco keyword", `eco`, 1},
		{"off keyword", `off`, 1},
		{"numeric quoted 0", `"mode":"0"`, 3},
		{"numeric quoted 1", `"mode":"1"`, 1},
		{"numeric quoted 2", `"mode":"2"`, 2},
		{"unknown", `{}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseModeFromResponse(tt.in))
		})
	}
}

func TestFindIAquaLinkDevice(t *testing.T) {
	log := util.NewLogger("test").Redact()
	devices := iaqualink.ListDevicesOutput{
		{ID: 42, Name: "Pool Heat Pump A", SerialNumber: "SN-001"},
		{ID: 99, Name: "Other", SerialNumber: "XYZ"},
	}

	id, sn, by := findIAquaLinkDevice(devices, "SN-001", log)
	assert.Equal(t, "42", id)
	assert.Equal(t, "SN-001", sn)
	assert.Equal(t, "serial number", by)

	id, sn, by = findIAquaLinkDevice(devices, "Heat", log)
	assert.Equal(t, "42", id)
	assert.Equal(t, "SN-001", sn)
	assert.Equal(t, "name", by)

	id, _, _ = findIAquaLinkDevice(devices, "missing", log)
	assert.Equal(t, "", id)
}

func TestProbeLocalCompatible(t *testing.T) {
	log := util.NewLogger("test").Redact()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"mode":"eco"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	mode, matched, err := ProbeLocalCompatible(ctx, srv.URL, log, 500*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, int64(1), mode)
	assert.Contains(t, matched, "/state")

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(empty.Close)

	_, _, err = ProbeLocalCompatible(ctx, empty.URL, log, 200*time.Millisecond)
	assert.ErrorIs(t, err, ErrLocalStateUnavailable)
}

func TestIaqualinkHTTPStatus(t *testing.T) {
	n, ok := iaqualinkHTTPStatus("received bad status code: 500")
	assert.True(t, ok)
	assert.Equal(t, 500, n)

	_, ok = iaqualinkHTTPStatus("connection refused")
	assert.False(t, ok)
}

func TestIsAPIErrorSuppressible(t *testing.T) {
	se401 := request.NewStatusError(&http.Response{
		StatusCode: http.StatusUnauthorized,
		Request:    &http.Request{Method: http.MethodGet, URL: mustURL(t, "http://example/devices")},
	})
	assert.True(t, isAPIErrorSuppressible(se401))

	se500 := request.NewStatusError(&http.Response{
		StatusCode: http.StatusInternalServerError,
		Request:    &http.Request{Method: http.MethodGet, URL: mustURL(t, "http://example/")},
	})
	assert.True(t, isAPIErrorSuppressible(se500))
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}
