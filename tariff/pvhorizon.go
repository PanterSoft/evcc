package tariff

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/nicomattes/pv-horizon/horizon"
	"github.com/nicomattes/pv-horizon/shading"
)

// PVHorizon is a solar tariff that applies terrain shading corrections.
// In standalone mode it returns capacity × geometric_factor × soft_factor (watts).
// In wrapper mode it multiplies an upstream tariff's rates by the shading factor.
type PVHorizon struct {
	log    *util.Logger
	data   *util.Monitor[api.Rates]
	pvSite horizon.Site
	pvOpts horizon.Options
	result *horizon.Result

	// standalone mode
	capacity float64 // peak panel capacity in watts
	tiltDeg  float64 // panel tilt from horizontal (0–90°)
	azimDeg  float64 // panel azimuth from North (0=N, 180=S)

	// wrapper mode
	upstream api.Tariff
}

var _ api.Tariff = (*PVHorizon)(nil)

func init() {
	registry.AddCtx("pvhorizon", NewPVHorizonFromConfig)
}

func NewPVHorizonFromConfig(ctx context.Context, other map[string]any) (api.Tariff, error) {
	cc := struct {
		Latitude  float64 `mapstructure:"latitude"`
		Longitude float64 `mapstructure:"longitude"`
		Elevation float64 `mapstructure:"elevation"`
		// standalone
		Capacity float64 `mapstructure:"capacity"` // peak watts
		Tilt     float64 `mapstructure:"tilt"`     // degrees from horizontal
		Azimuth  float64 `mapstructure:"azimuth"`  // degrees from North (180=South)
		// horizon options
		Radius   float64 `mapstructure:"radius"` // terrain search radius in metres
		CacheDir string  `mapstructure:"cache"`  // DEM tile cache directory
		// wrapper
		Upstream *Typed `mapstructure:"upstream"`
		// update
		Interval time.Duration `mapstructure:"interval"`
	}{
		Tilt:     30,
		Azimuth:  180,
		Interval: time.Hour,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.Latitude == 0 && cc.Longitude == 0 {
		return nil, fmt.Errorf("latitude and longitude are required")
	}

	switch {
	case cc.Capacity == 0 && cc.Upstream == nil:
		return nil, fmt.Errorf("either capacity (standalone mode) or upstream (wrapper mode) must be set")
	case cc.Capacity != 0 && cc.Upstream != nil:
		return nil, fmt.Errorf("capacity and upstream are mutually exclusive")
	}

	pvSite := horizon.Site{
		Lat:  cc.Latitude,
		Lon:  cc.Longitude,
		Elev: cc.Elevation,
	}

	pvOpts := horizon.Options{
		CacheDir:          cc.CacheDir,
		RadiusMeters:      cc.Radius,
		IncludeRefraction: true,
	}

	var upstream api.Tariff
	if cc.Upstream != nil {
		var err error
		upstream, err = NewFromConfig(ctx, cc.Upstream.Type, cc.Upstream.Other)
		if err != nil {
			return nil, fmt.Errorf("upstream: %w", err)
		}
	}

	return newPVHorizon(ctx, pvSite, pvOpts, cc.Capacity, cc.Tilt, cc.Azimuth, upstream, cc.Interval)
}

// newPVHorizon is the internal constructor; it accepts horizon.Options directly
// so tests can inject a fake DEM without network access.
func newPVHorizon(ctx context.Context, pvSite horizon.Site, pvOpts horizon.Options, capacity, tiltDeg, azimDeg float64, upstream api.Tariff, interval time.Duration) (api.Tariff, error) {
	log := util.NewLogger("pvhorizon")

	log.INFO.Printf("computing terrain horizon for (%.4f, %.4f)...", pvSite.Lat, pvSite.Lon)
	result, err := horizon.Compute(ctx, pvSite, pvOpts)
	if err != nil {
		return nil, fmt.Errorf("horizon compute: %w", err)
	}
	log.INFO.Printf("horizon ready: SVF=%.3f slope=%.1f° aspect=%.0f°",
		result.SVF, result.Ground.SlopeDeg, result.Ground.AspectDeg)

	t := &PVHorizon{
		log:      log,
		pvSite:   pvSite,
		pvOpts:   pvOpts,
		result:   result,
		capacity: capacity,
		tiltDeg:  tiltDeg,
		azimDeg:  azimDeg,
		upstream: upstream,
		data:     util.NewMonitor[api.Rates](2 * interval),
	}

	done := make(chan error)
	go t.run(done, interval)

	if err := <-done; err != nil {
		return nil, err
	}

	return t, nil
}

func (t *PVHorizon) run(done chan error, interval time.Duration) {
	var once sync.Once

	for ; true; <-time.Tick(interval) {
		var data api.Rates
		if err := backoff.Retry(func() error {
			var err error
			if t.upstream != nil {
				data, err = t.wrapperRates()
			} else {
				data, err = t.standaloneRates()
			}
			return backoffPermanentError(err)
		}, bo()); err != nil {
			once.Do(func() { done <- err })
			t.log.ERROR.Println(err)
			continue
		}

		mergeRatesAfter(t.data, data, beginningOfDay())
		once.Do(func() { close(done) })
	}
}

// standaloneRates generates solar power rates for the next 24 h using a
// geometric irradiance model adjusted for terrain shading.
func (t *PVHorizon) standaloneRates() (api.Rates, error) {
	now := time.Now().Truncate(SlotDuration)
	end := now.Add(24 * time.Hour)

	var times []time.Time
	for ts := now; ts.Before(end); ts = ts.Add(SlotDuration) {
		times = append(times, ts.Add(SlotDuration/2)) // midpoint of each slot
	}

	samples := shading.Evaluate(t.result.Profile, t.pvSite, times)

	data := make(api.Rates, len(samples))
	tiltRad := t.tiltDeg * math.Pi / 180
	panelAzRad := t.azimDeg * math.Pi / 180

	for i, s := range samples {
		slotStart := now.Add(time.Duration(i) * SlotDuration)
		power := 0.0
		if s.SoftFactor > 0 {
			sunElev := s.ElevationDeg * math.Pi / 180
			sunAz := s.AzimuthDeg * math.Pi / 180
			// cosine of incidence angle on tilted panel
			cosInc := math.Sin(sunElev)*math.Cos(tiltRad) +
				math.Cos(sunElev)*math.Sin(tiltRad)*math.Cos(sunAz-panelAzRad)
			if cosInc > 0 {
				power = t.capacity * cosInc * s.SoftFactor
			}
		}
		data[i] = api.Rate{
			Start: slotStart,
			End:   slotStart.Add(SlotDuration),
			Value: power,
		}
	}

	return data, nil
}

// wrapperRates fetches rates from the upstream tariff and scales each slot
// by the terrain shading soft factor at its midpoint.
func (t *PVHorizon) wrapperRates() (api.Rates, error) {
	upstream, err := t.upstream.Rates()
	if err != nil {
		return nil, err
	}

	// build midpoint timestamps for all upstream slots
	times := make([]time.Time, len(upstream))
	for i, r := range upstream {
		times[i] = r.Start.Add(r.End.Sub(r.Start) / 2)
	}

	samples := shading.Evaluate(t.result.Profile, t.pvSite, times)

	data := slices.Clone(upstream)
	for i, s := range samples {
		data[i].Value = upstream[i].Value * s.SoftFactor
	}

	return data, nil
}

// Rates implements api.Tariff.
func (t *PVHorizon) Rates() (api.Rates, error) {
	var res api.Rates
	err := t.data.GetFunc(func(val api.Rates) {
		res = slices.Clone(val)
	})
	return res, err
}

// Type implements api.Tariff.
func (t *PVHorizon) Type() api.TariffType {
	return api.TariffTypeSolar
}
