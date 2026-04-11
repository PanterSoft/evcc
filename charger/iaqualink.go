package charger

// LICENSE
//
// Copyright (c) 2024 andig
//
// This module is NOT covered by the MIT license. All rights reserved.
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/tekkamanendless/iaqualink"
)

func init() {
	registry.AddCtx("iaqualink", NewIAquaLinkFromConfig)
}

// iaqualinkConfig holds validated settings from NewIAquaLinkFromConfig.
type iaqualinkConfig struct {
	embed            *embed
	uri              string
	loginID          string
	password         string
	device           string
	allowUnsupported bool
	skipVerify       bool
	localMode        bool
}

type IAquaLink struct {
	*SgReady
	log              *util.Logger
	client           *iaqualink.Client // Cloud mode client
	helper           *request.Helper   // Local mode HTTP helper
	uri              string            // Local mode: device IP/URL
	deviceID         string            // Cloud mode: device ID
	deviceName       string            // Device name/identifier
	features         []string          // Available device features
	localMode        bool              // true if using local IP, false if using cloud
	readModeDisabled bool              // If true, skip mode reading attempts (API limitations)
	allowUnsupported bool              // User opted into degraded cloud behaviour
	mu               sync.Mutex
}

var _ api.ChargerEx = (*IAquaLink)(nil)

// NewIAquaLinkFromConfig creates an IAquaLink charger from generic config
func NewIAquaLinkFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := struct {
		embed            `mapstructure:",squash"`
		URI              string // Local mode: IP address or URL of the device
		Email, User      string // Cloud: login id (IAquaLink API JSON field "email"; User is an alias)
		Password         string
		Device           string
		AllowUnsupported bool `mapstructure:"allowunsupported"`
		SkipVerify       bool `mapstructure:"skipverify"` // Local: skip compatibility probe (not recommended)
	}{
		embed: embed{
			Icon_:     "heatpump",
			Features_: []api.Feature{api.Heating, api.IntegratedDevice},
		},
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	loginID := strings.TrimSpace(cc.Email)
	if loginID == "" {
		loginID = strings.TrimSpace(cc.User)
	}

	switch {
	case cc.URI != "" && (loginID != "" || cc.Password != ""):
		return nil, errors.New("cannot use both uri (local) and email/password (cloud) - choose one mode")
	case cc.URI != "":
		parsed, err := url.Parse(util.DefaultScheme(strings.TrimSpace(strings.TrimRight(cc.URI, "/")), "http"))
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("invalid uri for local mode: %q", cc.URI)
		}
		if cc.SkipVerify && cc.AllowUnsupported {
			return nil, errors.New("skipverify and allowunsupported cannot be combined (allowunsupported applies to cloud mode only)")
		}
	case loginID == "" || cc.Password == "":
		return nil, errors.New("must provide either uri (local mode) or user/email and password (cloud mode)")
	case strings.TrimSpace(cc.Device) == "":
		return nil, errors.New("device is required for cloud mode (serial number or name in IAquaLink)")
	}

	cfg := iaqualinkConfig{
		embed:            &cc.embed,
		uri:              strings.TrimSpace(cc.URI),
		loginID:          loginID,
		password:         cc.Password,
		device:           strings.TrimSpace(cc.Device),
		allowUnsupported: cc.AllowUnsupported,
		skipVerify:       cc.SkipVerify,
		localMode:        cc.URI != "",
	}

	return NewIAquaLink(ctx, cfg)
}

// NewIAquaLink creates IAquaLink charger from validated config.
func NewIAquaLink(ctx context.Context, cfg iaqualinkConfig) (api.Charger, error) {
	log := util.NewLogger("iaqualink").Redact(cfg.loginID, cfg.password, cfg.device, cfg.uri)

	c := &IAquaLink{
		SgReady:          nil,
		log:              log,
		deviceName:       cfg.device,
		allowUnsupported: cfg.allowUnsupported,
	}

	var err error
	if cfg.localMode {
		err = c.initLocal(ctx, cfg, log)
	} else {
		err = c.initCloud(ctx, cfg, log)
	}
	if err != nil {
		return nil, err
	}

	if !c.localMode && !c.allowUnsupported {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := c.getModeCloud(probeCtx); err != nil {
			return nil, fmt.Errorf("IAquaLink: could not read device mode (required for SG Ready); device may be unsupported: %w", err)
		}
	}

	log.INFO.Printf("IAquaLink device supports modes: Boost(3), Smart(2), Eco/Off(1) (mode: %s)", map[bool]string{true: "local", false: "cloud"}[c.localMode])

	setMode := func(mode int64) error {
		return c.setMode(ctx, mode)
	}

	getMode := func() (int64, error) {
		return c.getMode(ctx)
	}

	sgr, err := NewSgReady(ctx, cfg.embed, setMode, getMode, nil)
	if err != nil {
		return nil, err
	}

	c.SgReady = sgr

	return c, nil
}

func (c *IAquaLink) initLocal(ctx context.Context, cfg iaqualinkConfig, log *util.Logger) error {
	c.localMode = true
	c.uri = util.DefaultScheme(strings.TrimRight(cfg.uri, "/"), "http")
	c.helper = request.NewHelper(log)
	c.features = []string{iaqualink.FeatureModeInfo, iaqualink.FeatureStatus}

	log.INFO.Printf("IAquaLink using local mode: %s", c.uri)

	if cfg.skipVerify {
		log.WARN.Println("skipverify: skipping local compatibility probe; ensure the device matches IAquaLink local API expectations")
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.tryLocalStateEndpoints(probeCtx); err != nil {
		return fmt.Errorf("no compatible IAquaLink device at %s: %w", c.uri, err)
	}
	return nil
}

func (c *IAquaLink) initCloud(ctx context.Context, cfg iaqualinkConfig, log *util.Logger) error {
	c.localMode = false
	client := &iaqualink.Client{
		Client: request.NewClient(log),
	}

	loginOutput, err := client.Login(cfg.loginID, cfg.password)
	if err != nil {
		return fmt.Errorf("IAquaLink login failed: %w", err)
	}

	client.AuthenticationToken = loginOutput.AuthenticationToken
	if loginOutput.UserPoolOAuth.IDToken != "" {
		client.IDToken = loginOutput.UserPoolOAuth.IDToken
	}
	client.UserID = loginOutput.ID.String()

	devices, err := client.ListDevices()
	if err != nil {
		return fmt.Errorf("failed to list IAquaLink devices: %w", err)
	}

	deviceID, serialNumber, matchedBy := findIAquaLinkDevice(devices, cfg.device, log)
	if deviceID == "" {
		return fmt.Errorf("device not found in IAquaLink systems (tried matching by serial number and name)")
	}

	log.INFO.Printf("IAquaLink device matched by %s", matchedBy)

	deviceIdentifiers := []string{deviceID}
	if serialNumber != "" {
		deviceIdentifiers = append(deviceIdentifiers, serialNumber)
	}

	var featuresOutput *iaqualink.DeviceFeaturesOutput
	var featuresErr error
	for _, identifier := range deviceIdentifiers {
		log.DEBUG.Printf("Trying to get device features")
		featuresOutput, featuresErr = client.DeviceFeatures(identifier)
		if featuresErr == nil {
			log.DEBUG.Printf("Successfully got device features")
			deviceID = identifier
			break
		}
		log.DEBUG.Printf("Failed to get device features: %v", featuresErr)
	}

	if featuresErr != nil {
		if !cfg.allowUnsupported {
			if isLikelyNetworkError(featuresErr) {
				return fmt.Errorf("cannot reach IAquaLink device features API (temporary error): %w", featuresErr)
			}
			return fmt.Errorf("cannot read device capabilities; this device or account may be unsupported: %w", featuresErr)
		}
		log.WARN.Printf("allowunsupported: continuing without device features: %v", featuresErr)
		c.readModeDisabled = true
		featuresOutput = &iaqualink.DeviceFeaturesOutput{Features: []string{}}
	}

	c.client = client
	c.deviceID = deviceID
	if featuresOutput != nil {
		c.features = featuresOutput.Features
	}

	log.DEBUG.Printf("IAquaLink device features: %v", c.features)

	return nil
}

// setMode sets the device mode based on evcc SGReady mode (1/2/3)
func (c *IAquaLink) setMode(ctx context.Context, mode int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if mode < 1 || mode > 3 {
		return fmt.Errorf("invalid mode %d, expected 1/2/3", mode)
	}

	if c.localMode {
		return c.setModeLocal(ctx, mode)
	}
	return c.setModeCloud(ctx, mode)
}

// setModeCloud sets mode using cloud API
func (c *IAquaLink) setModeCloud(ctx context.Context, mode int64) error {
	actions := c.getActionsForMode(mode)
	if len(actions) == 0 {
		return fmt.Errorf("device does not support mode %d (available features: %v)", mode, c.features)
	}

	var lastErr error
	for _, action := range actions {
		c.log.DEBUG.Printf("Trying to set mode %d with action '%s'", mode, action)
		result, err := c.client.DeviceWebSocket(c.deviceID, action)
		if err == nil {
			c.log.DEBUG.Printf("Successfully set mode %d with action '%s', result: %v", mode, action, result)
			return nil
		}
		lastErr = err
		c.log.DEBUG.Printf("Action '%s' failed: %v, trying next", action, err)
	}

	return fmt.Errorf("failed to set IAquaLink mode %d with any action %v: %w", mode, actions, lastErr)
}

// setModeLocal sets mode using local IP API
// Note: Local API endpoints vary by device model/installation. This implementation
// tries common endpoint patterns, but may need device-specific configuration for some installations.
func (c *IAquaLink) setModeLocal(ctx context.Context, mode int64) error {
	modeCommands := map[int64]string{
		1: "eco",
		2: "smart",
		3: "boost",
	}

	command := modeCommands[mode]
	if command == "" {
		return fmt.Errorf("invalid mode %d for local API", mode)
	}

	endpoints := []string{
		fmt.Sprintf("%s/api/v1/mode", c.uri),
		fmt.Sprintf("%s/api/mode", c.uri),
		fmt.Sprintf("%s/mode", c.uri),
		fmt.Sprintf("%s/api/v1/command", c.uri),
	}

	data := map[string]string{"mode": command}
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("local mode command: %w", err)
	}

	var lastErr error
	for _, endpoint := range endpoints {
		c.log.DEBUG.Printf("Trying local endpoint: %s with command: %s", endpoint, command)

		req, err := request.New("POST", endpoint, strings.NewReader(string(body)), request.JSONEncoding)
		if err != nil {
			lastErr = err
			continue
		}
		req = req.WithContext(ctx)

		_, err = c.helper.DoBody(req)
		if err == nil {
			c.log.DEBUG.Printf("Successfully set mode %d via local API: %s", mode, endpoint)
			return nil
		}
		lastErr = err
		c.log.DEBUG.Printf("Local endpoint %s failed: %v", endpoint, err)
	}

	return fmt.Errorf("failed to set mode %d via local API: %w", mode, lastErr)
}

// getActionsForMode returns the IAquaLink actions for the given evcc mode
// based on available device features
func (c *IAquaLink) getActionsForMode(mode int64) []string {
	modeActions := map[int64][]string{
		1: {"eco", "off"},
		2: {"smart", "normal"},
		3: {"boost"},
	}

	return modeActions[mode]
}

// getMode gets the current device mode as evcc SGReady mode (1/2/3)
func (c *IAquaLink) getMode(ctx context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.localMode {
		return c.getModeLocal(ctx)
	}
	return c.getModeCloud(ctx)
}

// getModeCloud gets mode using cloud API
func (c *IAquaLink) getModeCloud(ctx context.Context) (int64, error) {
	if c.client == nil {
		return 0, errors.New("IAquaLink client not initialized")
	}

	if c.readModeDisabled {
		return 0, errors.New("device mode could not be read (API limitation for this device)")
	}

	site, err := c.client.DeviceSite(c.deviceID)
	if err != nil {
		if !isAPIErrorSuppressible(err) {
			c.log.DEBUG.Printf("DeviceSite failed: %v", err)
		}
	} else if site != nil {
		_ = site
	}

	commandsToTry := []string{"state", "status"}
	if c.hasFeature(iaqualink.FeatureModeInfo) {
		commandsToTry = append([]string{"mode_info"}, commandsToTry...)
	}

	for _, cmd := range commandsToTry {
		values := url.Values{}
		values.Set(cmd, "1")

		output, err := c.client.DeviceExecuteReadCommand(c.deviceID, values)
		if err != nil {
			if !isAPIErrorSuppressible(err) {
				c.log.DEBUG.Printf("DeviceExecuteReadCommand '%s' failed: %v", cmd, err)
			}
			continue
		}
		if output != nil && output.Command.Response != "" {
			if mode := parseModeFromResponse(output.Command.Response); mode > 0 {
				c.log.DEBUG.Printf("Got mode %d from command '%s'", mode, cmd)
				return mode, nil
			}
		}
	}

	c.log.DEBUG.Printf("Could not determine device mode from any method")
	if c.readModeDisabled {
		return 0, errors.New("device mode could not be read (API limitation for this device)")
	}
	return 0, errors.New("could not read device mode from IAquaLink API")
}

// ErrLocalStateUnavailable is returned when no known local path returned parseable IAquaLink state.
var ErrLocalStateUnavailable = errors.New("no supported endpoint responded with valid state")

func localStateEndpointURLs(baseURI string) []string {
	baseURI = strings.TrimRight(util.DefaultScheme(strings.TrimSpace(baseURI), "http"), "/")
	return []string{
		fmt.Sprintf("%s/api/v1/state", baseURI),
		fmt.Sprintf("%s/api/state", baseURI),
		fmt.Sprintf("%s/state", baseURI),
		fmt.Sprintf("%s/api/v1/status", baseURI),
		fmt.Sprintf("%s/api/status", baseURI),
		fmt.Sprintf("%s/status", baseURI),
	}
}

// probeLocalStateEndpoints tries common local API GET paths. helper must use an appropriate Client.Timeout for the use case.
func probeLocalStateEndpoints(ctx context.Context, helper *request.Helper, log *util.Logger, baseURI string) (mode int64, matchedEndpoint string, err error) {
	baseURI = strings.TrimRight(util.DefaultScheme(strings.TrimSpace(baseURI), "http"), "/")

	for _, endpoint := range localStateEndpointURLs(baseURI) {
		if log != nil {
			log.DEBUG.Printf("Trying local endpoint: %s", endpoint)
		}

		req, err := request.New("GET", endpoint, nil, request.AcceptJSON)
		if err != nil {
			continue
		}
		req = req.WithContext(ctx)

		respBody, err := helper.DoBody(req)
		if err == nil && len(respBody) > 0 {
			if m := parseModeFromResponse(string(respBody)); m > 0 {
				if log != nil {
					log.DEBUG.Printf("Got mode %d from local endpoint: %s", m, endpoint)
				}
				return m, endpoint, nil
			}
		}
	}

	return 0, "", ErrLocalStateUnavailable
}

// ProbeLocalCompatible runs GET requests on common IAquaLink local paths (same as charger init).
// perRequestTimeout bounds each HTTP round trip; use ~400ms for evcc detect scans.
func ProbeLocalCompatible(ctx context.Context, baseURI string, log *util.Logger, perRequestTimeout time.Duration) (mode int64, matchedEndpoint string, err error) {
	client := &http.Client{Timeout: perRequestTimeout}
	helper := &request.Helper{Client: client}
	return probeLocalStateEndpoints(ctx, helper, log, baseURI)
}

// tryLocalStateEndpoints tries common local API endpoints to read device state.
// Returns (mode, nil) if any endpoint returns parseable IAquaLink state, otherwise (0, err).
func (c *IAquaLink) tryLocalStateEndpoints(ctx context.Context) (int64, error) {
	mode, _, err := probeLocalStateEndpoints(ctx, c.helper, c.log, c.uri)
	return mode, err
}

// getModeLocal gets mode using local IP API
// Note: Local API endpoints vary by device model/installation. This implementation
// tries common endpoint patterns, but may need device-specific configuration for some installations.
func (c *IAquaLink) getModeLocal(ctx context.Context) (int64, error) {
	mode, err := c.tryLocalStateEndpoints(ctx)
	if err != nil {
		return 0, fmt.Errorf("could not read mode from device: %w", err)
	}
	return mode, nil
}

// parseModeFromResponse parses mode from device response string.
// Heuristic only: responses vary by firmware; ambiguous JSON may be misclassified.
func parseModeFromResponse(response string) int64 {
	stateStr := strings.ToLower(response)

	if strings.Contains(stateStr, "boost") {
		return 3
	}

	if strings.Contains(stateStr, "smart") || strings.Contains(stateStr, "normal") {
		return 2
	}

	if strings.Contains(stateStr, "eco") || strings.Contains(stateStr, "off") {
		return 1
	}

	if strings.Contains(stateStr, "\"0\"") || strings.Contains(stateStr, ":0") {
		return 3
	}
	if strings.Contains(stateStr, "\"1\"") || strings.Contains(stateStr, ":1") {
		return 1
	}
	if strings.Contains(stateStr, "\"2\"") || strings.Contains(stateStr, ":2") {
		return 2
	}

	return 0
}

// hasFeature checks if device has a specific feature
func (c *IAquaLink) hasFeature(feature string) bool {
	return slices.Contains(c.features, feature)
}

// findIAquaLinkDevice searches for a device in the list by serial number or name.
func findIAquaLinkDevice(devices iaqualink.ListDevicesOutput, device string, log *util.Logger) (deviceID, serialNumber, matchedBy string) {
	deviceLower := strings.ToLower(device)

	for _, dev := range devices {
		if strings.EqualFold(dev.SerialNumber, device) {
			log.DEBUG.Printf("Found device by serial number: ID=%d", dev.ID)
			return strconv.Itoa(dev.ID), dev.SerialNumber, "serial number"
		}
	}

	for _, dev := range devices {
		if strings.Contains(strings.ToLower(dev.Name), deviceLower) {
			log.DEBUG.Printf("Found device by name: ID=%d", dev.ID)
			return strconv.Itoa(dev.ID), dev.SerialNumber, "name"
		}
	}

	return "", "", ""
}

// isLikelyNetworkError returns true when the failure is probably transport-level (retry may help).
func isLikelyNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	errStr := err.Error()
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no route to host") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "TLS handshake") ||
		strings.Contains(errStr, "EOF") {
		return true
	}
	// iaqualink Raw returns HTTP status in-band for non-2xx
	if strings.Contains(errStr, "bad status code:") {
		return false
	}
	return false
}

// isAPIErrorSuppressible checks if an error should be logged at DEBUG only when probing cloud APIs.
func isAPIErrorSuppressible(err error) bool {
	if err == nil {
		return false
	}
	var se *request.StatusError
	if errors.As(err, &se) {
		return se.HasStatus(401, 500, 502, 503, 504)
	}
	errStr := err.Error()
	code, ok := iaqualinkHTTPStatus(errStr)
	if ok {
		return code == 401 || code == 500 || code == 502 || code == 503 || code == 504
	}
	return strings.Contains(errStr, "UNAUTHORIZED") || strings.Contains(errStr, "SERVER_ERROR")
}

// iaqualinkHTTPStatus parses "received bad status code: NNN" from iaqualink client errors.
func iaqualinkHTTPStatus(s string) (int, bool) {
	const prefix = "received bad status code: "
	if !strings.Contains(s, prefix) {
		return 0, false
	}
	idx := strings.LastIndex(s, prefix)
	if idx < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s[idx+len(prefix):]))
	if err != nil {
		return 0, false
	}
	return n, true
}
