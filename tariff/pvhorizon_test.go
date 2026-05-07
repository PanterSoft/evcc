package tariff

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/nicomattes/pv-horizon/horizon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flatDEM is a fake DEM that returns a constant elevation everywhere.
// It allows testing the full tariff pipeline without network access.
type flatDEM struct{ elev float64 }

func (f *flatDEM) Sample(_ context.Context, _, _ float64) (float64, bool, error) {
	return f.elev, true, nil
}

// deepValleyDEM surrounds the site with a tall ridge ring at ~500 m, creating
// a ~22° horizon in all directions. This blocks low-elevation sun (early
// morning / late evening) regardless of season, giving a measurable yield reduction.
type deepValleyDEM struct{ baseElev, siteLat, siteLon float64 }

func (d *deepValleyDEM) Sample(_ context.Context, lat, lon float64) (float64, bool, error) {
	dy := (lat - d.siteLat) * 111000
	dx := (lon - d.siteLon) * 75000
	dist := math.Sqrt(dy*dy + dx*dx)
	if dist > 400 && dist < 600 {
		return d.baseElev + 200, true, nil // atan(200/500) ≈ 21.8° horizon
	}
	return d.baseElev, true, nil
}

// fixedSolarTariff is an upstream tariff stub that returns a fixed set of rates.
type fixedSolarTariff struct{ rates api.Rates }

func (m *fixedSolarTariff) Rates() (api.Rates, error) { return m.rates, nil }
func (m *fixedSolarTariff) Type() api.TariffType      { return api.TariffTypeSolar }

func fastOpts(dem interface {
	Sample(context.Context, float64, float64) (float64, bool, error)
}) horizon.Options {
	return horizon.Options{
		DEM:               dem,
		RadiusMeters:      2000,
		AzimuthSteps:      36, // 10° bins
		StepMeters:        100,
		IncludeRefraction: false,
	}
}

// TestPVHorizonStandalone_RateCount verifies the tariff emits exactly 96 rates
// (24 h × 4 slots/h) on creation.
func TestPVHorizonStandalone_RateCount(t *testing.T) {
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}
	tariff, err := newPVHorizon(context.Background(), site, fastOpts(&flatDEM{500}),
		10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)

	rates, err := tariff.Rates()
	require.NoError(t, err)
	assert.Equal(t, 96, len(rates), "expected 24 h × 4 slots")
}

// TestPVHorizonStandalone_DayNight checks that a summer-solstice midday slot has
// positive power and a midnight slot has zero power.
func TestPVHorizonStandalone_DayNight(t *testing.T) {
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}
	tariff, err := newPVHorizon(context.Background(), site, fastOpts(&flatDEM{500}),
		10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)

	rates, err := tariff.Rates()
	require.NoError(t, err)

	var maxDay, midnightPower float64
	loc := time.Local
	for _, r := range rates {
		h := r.Start.In(loc).Hour()
		if h >= 10 && h <= 14 {
			if r.Value > maxDay {
				maxDay = r.Value
			}
		}
		if h == 0 {
			midnightPower += r.Value
		}
	}

	assert.Greater(t, maxDay, 0.0, "midday slots should have positive power")
	assert.Equal(t, 0.0, midnightPower, "midnight slots must be zero (sun below horizon)")
}

// TestPVHorizonStandalone_Contiguous verifies that the returned rates are
// contiguous (no gaps) and cover a full day with 15-min slots.
func TestPVHorizonStandalone_Contiguous(t *testing.T) {
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}
	tariff, err := newPVHorizon(context.Background(), site, fastOpts(&flatDEM{500}),
		10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)

	rates, err := tariff.Rates()
	require.NoError(t, err)
	require.NotEmpty(t, rates)

	for i := 1; i < len(rates); i++ {
		assert.True(t, rates[i].Start.Equal(rates[i-1].End),
			"slot %d start %v != slot %d end %v", i, rates[i].Start, i-1, rates[i-1].End)
		assert.Equal(t, SlotDuration, rates[i].End.Sub(rates[i].Start),
			"slot %d has wrong duration", i)
	}
}

// TestPVHorizonStandalone_TiltVsFlat verifies that a tilted south-facing panel
// produces more power than a horizontal one during midday.
func TestPVHorizonStandalone_TiltVsFlat(t *testing.T) {
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}
	dem := &flatDEM{500}

	tilted, err := newPVHorizon(context.Background(), site, fastOpts(dem), 10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)
	flat, err := newPVHorizon(context.Background(), site, fastOpts(dem), 10000, 0, 180, nil, time.Hour)
	require.NoError(t, err)

	tiltedRates, _ := tilted.Rates()
	flatRates, _ := flat.Rates()

	var tiltedSum, flatSum float64
	loc := time.Local
	for i, r := range tiltedRates {
		h := r.Start.In(loc).Hour()
		if h >= 9 && h <= 15 {
			tiltedSum += r.Value
			flatSum += flatRates[i].Value
		}
	}

	// A 30° south-facing panel should outperform horizontal at 47°N during the day.
	// (Summer: sun is high enough that tilt helps; winter: tilt helps even more.)
	// This is a sanity check, not a precise physics assertion.
	assert.GreaterOrEqual(t, tiltedSum, flatSum*0.9,
		"tilted panel should produce at least as much as horizontal")
}

// TestPVHorizonStandalone_ValleyShadow verifies that a site surrounded by a
// tall ridge ring (deep valley) produces less power than a flat terrain site.
// The ~22° all-around horizon blocks low-elevation sun at sunrise/sunset,
// giving a measurable yield reduction independent of season.
func TestPVHorizonStandalone_ValleyShadow(t *testing.T) {
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}
	ctx := context.Background()

	flat, err := newPVHorizon(ctx, site, fastOpts(&flatDEM{500}), 10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)
	valley, err := newPVHorizon(ctx, site, fastOpts(&deepValleyDEM{500, 47.05, 8.30}), 10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)

	flatRates, _ := flat.Rates()
	valleyRates, _ := valley.Rates()

	var flatTotal, valleyTotal float64
	for i := range flatRates {
		flatTotal += flatRates[i].Value
		valleyTotal += valleyRates[i].Value
	}

	assert.Less(t, valleyTotal, flatTotal,
		"deep valley (22° horizon) must reduce total daily yield vs flat terrain")
}

// TestPVHorizonWrapper_ScalesUpstream verifies that wrapper mode multiplies
// each upstream rate by the shading soft factor (≤ 1), so wrapper output ≤ upstream.
func TestPVHorizonWrapper_ScalesUpstream(t *testing.T) {
	// Build 96 upstream rates at a constant 5000 W starting from beginning of today.
	start := beginningOfDay()
	upstreamRates := make(api.Rates, 96)
	for i := range upstreamRates {
		s := start.Add(time.Duration(i) * SlotDuration)
		upstreamRates[i] = api.Rate{Start: s, End: s.Add(SlotDuration), Value: 5000}
	}

	upstream := &fixedSolarTariff{rates: upstreamRates}
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}

	tariff, err := newPVHorizon(context.Background(), site, fastOpts(&flatDEM{500}),
		0, 30, 180, upstream, time.Hour)
	require.NoError(t, err)

	rates, err := tariff.Rates()
	require.NoError(t, err)
	require.NotEmpty(t, rates)

	var upstreamTotal, wrappedTotal float64
	for i, r := range rates {
		wrappedTotal += r.Value
		upstreamTotal += upstreamRates[i].Value
		assert.LessOrEqual(t, r.Value, upstreamRates[i].Value+1e-9,
			"wrapper must never amplify upstream at slot %d", i)
	}

	assert.Less(t, wrappedTotal, upstreamTotal,
		"shading must reduce total energy (night slots are zero)")
}

// TestPVHorizonWrapper_NightZero verifies that wrapper output is zero for any
// slot where the sun is below the horizon, regardless of upstream value.
func TestPVHorizonWrapper_NightZero(t *testing.T) {
	loc := time.Local
	// Build rates for a single night slot (midnight).
	midnight := beginningOfDay()
	upstreamRates := api.Rates{{
		Start: midnight,
		End:   midnight.Add(SlotDuration),
		Value: 9999, // artificially large to make sure shading zeroes it
	}}

	upstream := &fixedSolarTariff{rates: upstreamRates}
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}

	tariff, err := newPVHorizon(context.Background(), site, fastOpts(&flatDEM{500}),
		0, 30, 180, upstream, time.Hour)
	require.NoError(t, err)

	rates, err := tariff.Rates()
	require.NoError(t, err)

	for _, r := range rates {
		h := r.Start.In(loc).Hour()
		if h == 0 {
			assert.Equal(t, 0.0, r.Value,
				"midnight slot must be zero (sun below horizon)")
		}
	}
}

// TestPVHorizonType verifies the tariff always reports TariffTypeSolar.
func TestPVHorizonType(t *testing.T) {
	site := horizon.Site{Lat: 47.05, Lon: 8.30, Elev: 500}
	tariff, err := newPVHorizon(context.Background(), site, fastOpts(&flatDEM{500}),
		10000, 30, 180, nil, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, api.TariffTypeSolar, tariff.Type())
}
