package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/opticaltransport/otp/receiver/ai/soft"
)

// Settings is what a configuration says about the recovery engine.
//
// Its own type rather than the receiver's config struct, so this package does not depend on the
// receiver's configuration package — which would make the engine tree unusable from a benchmark or a
// corpus analysis that has no config to hand. The caller translates.
type Settings struct {
	// Enabled turns recovery off entirely, which yields Null rather than a nil engine.
	Enabled bool

	// Engine names the composition: "go" for the deterministic search alone, "sidecar" for a model
	// server with the deterministic search behind it, or "none".
	Engine string

	// Search bounds the deterministic candidate search.
	Search soft.Options

	// SidecarURL and SidecarTimeout configure the model server, when one is named.
	SidecarURL     string
	SidecarTimeout time.Duration
}

// AvailableEngines is what this build can open, for configuration validation and the settings UI.
//
// A list rather than a free-form string check, for the same reason the capture sources are enumerated:
// accepting a name and failing later is how a settings page comes to be able to stop a service.
func AvailableEngines() []string { return []string{"none", "go", "sidecar"} }

// Open builds the engine a configuration names.
//
// A sidecar that cannot be reached is an error rather than a silent downgrade to the deterministic
// search. An operator who configured a model server and got the baseline instead would see recovery
// numbers that looked plausible and were measuring something else entirely — and there would be nothing
// in them to say so. Refusing is louder and cheaper.
func Open(ctx context.Context, s Settings) (Engine, error) {
	if !s.Enabled {
		return NewNull(), nil
	}

	switch s.Engine {
	case "", "go":
		return NewGo(s.Search), nil

	case "none":
		return NewNull(), nil

	case "sidecar":
		// The deterministic search sits behind the model server, both as the chain's cheap first rung
		// and as the finisher on the enhanced image.
		inner := NewGo(s.Search)
		side, err := NewSidecar(ctx, SidecarOptions{
			URL:     s.SidecarURL,
			Timeout: s.SidecarTimeout,
			Inner:   inner,
		})
		if err != nil {
			return nil, err
		}
		// Cheapest first: the search on the original pixels, then the model server.
		return NewChain(inner, side), nil

	default:
		return nil, fmt.Errorf("engine: %q is not one of %v", s.Engine, AvailableEngines())
	}
}
