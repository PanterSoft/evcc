package charger

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIAquaLinkModes verifies that every SG Ready mode maps to a valid device
// mode and that the mapping is reversible, as getMode relies on lo.FindKey.
func TestIAquaLinkModes(t *testing.T) {
	for _, mode := range []int64{Dim, Normal, Boost} {
		hpMode, ok := sgReadyModes[mode]
		require.True(t, ok, "no device mode for SG Ready mode %d", mode)
		assert.True(t, hpMode.Valid(), "invalid device mode %s for SG Ready mode %d", hpMode, mode)

		res, ok := lo.FindKey(sgReadyModes, hpMode)
		require.True(t, ok)
		assert.Equal(t, mode, res)
	}
}
