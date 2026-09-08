package charger

// LICENSE

// Copyright (c) evcc.io (andig, naltatis, premultiply)

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	iaqualink "github.com/PanterSoft/iAqualink_go"
	"github.com/PanterSoft/iAqualink_go/heatpump"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/api/implement"
	"github.com/evcc-io/evcc/util"
	"github.com/samber/lo"
)

func init() {
	registry.AddCtx("iaqualink", NewIAquaLinkFromConfig)
}

// IAquaLink is an SG Ready charger for Zodiac zs500 pool heat pumps on the iAqualink cloud
type IAquaLink struct {
	*SgReady
	hp    *heatpump.HeatPump
	state func() (*heatpump.State, error)

	ecoPower, boostPower int64

	mu      sync.Mutex
	sgMode  int64         // last mode requested by evcc
	power   int64         // last power offered by evcc
	applied heatpump.Mode // last mode written to the device
	valid   bool          // applied is meaningful
}

// NewIAquaLinkFromConfig creates an IAquaLink charger from generic config
func NewIAquaLinkFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := struct {
		embed          `mapstructure:",squash"`
		User, Password string
		Device         string
		Cache          time.Duration
		EcoPower       int64 `mapstructure:"ecopower"`
		BoostPower     int64 `mapstructure:"boostpower"`
	}{
		embed: embed{
			Icon_:     "heatpump",
			Features_: []api.Feature{api.Continuous, api.Heating, api.IntegratedDevice},
		},
		// the cloud API rate-limits below roughly one request per 10s
		Cache:      30 * time.Second,
		EcoPower:   1000,
		BoostPower: 2000,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.User == "" || cc.Password == "" {
		return nil, api.ErrMissingCredentials
	}

	if cc.EcoPower >= cc.BoostPower {
		return nil, fmt.Errorf("ecopower (%dW) must be below boostpower (%dW)", cc.EcoPower, cc.BoostPower)
	}

	log := util.NewLogger("iaqualink").Redact(cc.User, cc.Password, cc.Device)

	client, err := iaqualink.NewClient(ctx, cc.User, cc.Password)
	if err != nil {
		return nil, err
	}

	dev, err := ensureEx("device", cc.Device, func() ([]iaqualink.DeviceInfo, error) {
		devices, err := client.ListDevices(ctx)
		if err != nil {
			return nil, err
		}
		return lo.Filter(devices, func(d iaqualink.DeviceInfo, _ int) bool {
			return d.DeviceType == iaqualink.DeviceTypeZS500
		}), nil
	}, func(d iaqualink.DeviceInfo) (string, error) {
		return d.SerialNumber, nil
	})
	if err != nil {
		return nil, err
	}

	hp, err := heatpump.New(dev, client.Session())
	if err != nil {
		return nil, err
	}

	c := &IAquaLink{
		hp:         hp,
		ecoPower:   cc.EcoPower,
		boostPower: cc.BoostPower,
		state: util.Cached(func() (*heatpump.State, error) {
			return hp.GetState(ctx)
		}, cc.Cache),
	}

	state, err := c.state()
	if err != nil {
		return nil, err
	}

	if !state.Power {
		log.WARN.Println("device is switched off - evcc controls the operating mode only and will not switch it on")
	}

	// the mode getter is nil on purpose: the device mode is derived from the offered
	// power, so reading it back would not round-trip to the requested SG Ready mode
	// and Enabled() would contradict the last Enable() call
	c.SgReady, err = NewSgReady(ctx, &cc.embed, func(mode int64) error {
		return c.setMode(ctx, mode)
	}, nil, func(power int64) error {
		return c.setMaxPower(ctx, power)
	})
	if err != nil {
		return nil, err
	}

	// water temperature and the device's own setpoint, both from the cached state
	implement.Has(c, implement.Battery(c.temp))
	implement.Has(c, implement.SocLimiter(c.limitTemp))

	return c, nil
}

// setMode records the SG Ready mode requested by evcc
func (c *IAquaLink) setMode(ctx context.Context, mode int64) error {
	c.mu.Lock()
	c.sgMode = mode
	c.mu.Unlock()

	return c.applyMode(ctx)
}

// setMaxPower records the power evcc offers the device
func (c *IAquaLink) setMaxPower(ctx context.Context, power int64) error {
	c.mu.Lock()
	c.power = power
	c.mu.Unlock()

	return c.applyMode(ctx)
}

// deviceMode picks the device mode from the SG Ready mode and the offered power.
//
// The device modes are efficiency levels rather than output steps: eco heats slowly
// and quietly, smart looks for the best operating point, boost heats as fast as it
// can regardless of consumption. Selecting them by the available power therefore runs
// the pump at the efficiency its energy budget allows.
//
// Only the enabled state modulates: SG Ready normal leaves the unit in its own adaptive
// smart program and dim (§14a) drops it to eco. The unit is never switched off, as
// power-cycling short-cycles its compressor.
func (c *IAquaLink) deviceMode() heatpump.Mode {
	switch {
	case c.sgMode == Dim:
		// §14a curtailment: least consumption the unit offers
		return heatpump.ModeEco
	case c.sgMode != Boost:
		// SG Ready normal operation - the unit runs its own adaptive program
		return heatpump.ModeSmart
	}

	switch {
	case c.power < c.ecoPower:
		return heatpump.ModeEco
	case c.power < c.boostPower:
		return heatpump.ModeSmart
	default:
		return heatpump.ModeBoost
	}
}

// applyMode writes the derived device mode, skipping unchanged writes as the
// cloud API is rate-limited
func (c *IAquaLink) applyMode(ctx context.Context) error {
	c.mu.Lock()
	mode := c.deviceMode()
	unchanged := c.valid && mode == c.applied
	c.mu.Unlock()

	if unchanged {
		return nil
	}

	if err := c.hp.SetMode(ctx, mode); err != nil {
		return err
	}

	c.mu.Lock()
	c.applied, c.valid = mode, true
	c.mu.Unlock()

	return nil
}

// temp implements the api.Battery interface and returns the water temperature
func (c *IAquaLink) temp() (float64, error) {
	state, err := c.state()
	if err != nil {
		return 0, err
	}

	if !state.WaterOK {
		return 0, api.ErrNotAvailable
	}

	return state.WaterTemp, nil
}

// limitTemp implements the api.SocLimiter interface and returns the device's target temperature
func (c *IAquaLink) limitTemp() (int64, error) {
	state, err := c.state()
	if err != nil {
		return 0, err
	}

	return int64(math.Round(state.TargetTemp)), nil
}
