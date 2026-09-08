package charger

import (
	"testing"

	"github.com/PanterSoft/iAqualink_go/heatpump"
	"github.com/stretchr/testify/assert"
)

// TestIAquaLinkDeviceMode verifies that the offered power selects the device
// efficiency mode, and that only the enabled state modulates.
func TestIAquaLinkDeviceMode(t *testing.T) {
	c := &IAquaLink{ecoPower: 1000, boostPower: 2000}

	for _, tc := range []struct {
		sgMode   int64
		power    int64
		expected heatpump.Mode
	}{
		{Boost, 0, heatpump.ModeEco},
		{Boost, 999, heatpump.ModeEco},
		{Boost, 1000, heatpump.ModeSmart},
		{Boost, 1999, heatpump.ModeSmart},
		{Boost, 2000, heatpump.ModeBoost},
		{Boost, 5000, heatpump.ModeBoost},
		// disabled and §14a dim idle the device regardless of power
		{Normal, 5000, heatpump.ModeEco},
		{Dim, 5000, heatpump.ModeEco},
	} {
		c.sgMode, c.power = tc.sgMode, tc.power
		assert.Equal(t, tc.expected, c.deviceMode(), "sgMode %d, power %dW", tc.sgMode, tc.power)
	}
}
