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
}

// NewIAquaLinkFromConfig creates an IAquaLink charger from generic config
func NewIAquaLinkFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := struct {
		embed          `mapstructure:",squash"`
		User, Password string
		Device         string
		Cache          time.Duration
	}{
		embed: embed{
			Icon_:     "heatpump",
			Features_: []api.Feature{api.Heating, api.IntegratedDevice},
		},
		// the cloud API rate-limits below roughly one request per 10s
		Cache: 30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.User == "" || cc.Password == "" {
		return nil, api.ErrMissingCredentials
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
		hp: hp,
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

	c.SgReady, err = NewSgReady(ctx, &cc.embed, func(mode int64) error {
		return c.setMode(ctx, mode)
	}, c.getMode, nil)
	if err != nil {
		return nil, err
	}

	// water temperature and the device's own setpoint, both from the cached state
	implement.Has(c, implement.Battery(c.temp))
	implement.Has(c, implement.SocLimiter(c.limitTemp))

	return c, nil
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

// sgReadyModes maps evcc SG Ready modes to iAqualink heat pump modes.
// There is no mapping for switching the device off: SG Ready Dim asks for reduced
// operation, and power-cycling the unit short-cycles its compressor.
var sgReadyModes = map[int64]heatpump.Mode{
	Dim:    heatpump.ModeEco,
	Normal: heatpump.ModeSmart,
	Boost:  heatpump.ModeBoost,
}

func (c *IAquaLink) setMode(ctx context.Context, mode int64) error {
	hpMode, ok := sgReadyModes[mode]
	if !ok {
		return fmt.Errorf("invalid mode: %d", mode)
	}

	return c.hp.SetMode(ctx, hpMode)
}

func (c *IAquaLink) getMode() (int64, error) {
	state, err := c.state()
	if err != nil {
		return 0, err
	}

	mode, ok := lo.FindKey(sgReadyModes, state.Mode)
	if !ok {
		return 0, fmt.Errorf("unknown device mode: %s", state.Mode)
	}

	return mode, nil
}
