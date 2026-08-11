package api

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/sender/internal/config"
)

// TestEverySettingTheAPICanChangeCanAlsoBeStored is a drift guard, and it is guarding against this round's
// bug reappearing by a different route.
//
// The settings API changed the running configuration and stored nothing, so the display sink — read only at
// startup — was discarded by the restart it needed. The overlay fixes that, but only for the keys it knows.
// A field added to settingsRequest and forgotten in config.SettingKeys would apply immediately, look correct,
// and then vanish on the next restart: the same silent revert, in one field instead of eleven.
//
// Comparing the request's own json tags against the storable keys keeps the two honest without anyone having
// to remember.
func TestEverySettingTheAPICanChangeCanAlsoBeStored(t *testing.T) {
	var fields []string
	requestType := reflect.TypeOf(settingsRequest{})
	for i := 0; i < requestType.NumField(); i++ {
		tag := requestType.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fields = append(fields, name)
	}
	require.NotEmpty(t, fields, "the request struct should have json-tagged fields")

	storable := config.SettingKeys()
	sort.Strings(fields)
	sort.Strings(storable)

	require.Equal(t, fields, storable,
		"settingsRequest and config.SettingKeys have drifted: a setting the API can change but cannot store "+
			"applies immediately and is silently lost on the next restart")
}
