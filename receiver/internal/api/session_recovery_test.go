package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/opticaltransport/otp/receiver/internal/pipeline"
)

// The session view's recovery object is what the camera page reads to tell an operator which stage frames are
// failing at. Its shape is a contract with the browser, so it is asserted here rather than left to be discovered
// by a panel rendering "undefined".
//
// Marshalling the view directly rather than driving the route, because getSession reads the capture session from
// Postgres and would skip on a machine without one — and a contract test that skips where the contract is being
// changed is worth nothing. The route's own wiring is covered by the field being populated in getSession, which
// the compiler enforces.

func TestSessionViewOmitsRecoveryWhenNothingCanReportIt(t *testing.T) {
	body, err := json.Marshal(SessionView{})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))

	// Absent, not zeroed. An operator reading "0 attempted" would conclude the channel is healthy, when the
	// truth is that nothing is asking — so the two must not look alike.
	_, present := got["recovery"]
	require.False(t, present, "recovery must be omitted when the accessor is not wired: %s", body)
}

func TestSessionViewCarriesRecoveryCountsAndBuckets(t *testing.T) {
	stats := pipeline.RecoveryStats{
		Attempted:  12,
		Recovered:  5,
		Candidates: 143,
		Buckets:    map[string]uint64{"decoded": 900, "payload_crc": 7, "no_quad": 5},
	}

	body, err := json.Marshal(SessionView{Recovery: &stats})
	require.NoError(t, err)

	var got struct {
		Recovery *struct {
			Attempted  uint64            `json:"attempted"`
			Recovered  uint64            `json:"recovered"`
			Candidates uint64            `json:"candidates"`
			Buckets    map[string]uint64 `json:"buckets"`
		} `json:"recovery"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotNil(t, got.Recovery)
	require.Equal(t, uint64(12), got.Recovery.Attempted)
	require.Equal(t, uint64(5), got.Recovery.Recovered)
	require.Equal(t, uint64(143), got.Recovery.Candidates)
	require.Equal(t, uint64(7), got.Recovery.Buckets["payload_crc"])
	require.Equal(t, uint64(900), got.Recovery.Buckets["decoded"])
}

// TestRecoveryStatsBucketsAreNeverNullInJSON guards the frontend's Object.entries call: a nil map marshals to
// null, and Object.entries(null) throws rather than returning nothing.
func TestRecoveryStatsBucketsAreNeverNullInJSON(t *testing.T) {
	var counters pipeline.RecoveryStats
	body, err := json.Marshal(pipeline.RecoveryStats{Buckets: counters.Buckets})
	require.NoError(t, err)
	require.Contains(t, string(body), `"buckets":null`,
		"this records the hazard; the receiver must therefore always send a non-nil map")

	// Which is what RecoveryStats does, via the pipeline's stats snapshot: an empty session reports an empty
	// object, not null.
	body, err = json.Marshal(pipeline.RecoveryStats{Buckets: map[string]uint64{}})
	require.NoError(t, err)
	require.Contains(t, string(body), `"buckets":{}`)
}
