// Package encoding implements the optical encodings that carry a frame's payload
// across the grid, behind a plug-in interface.
//
// Only the payload region varies between encodings. The fiducials, timing
// columns, grid descriptor, header band, and footer band are always rendered in
// plain binary, whatever modulation the payload uses, because a decoder has to be
// able to read the header before it knows which encoding produced the frame.
package encoding

import (
	"errors"
	"fmt"
	"image"

	"github.com/opticaltransport/otp/shared/internal/registry"
	"github.com/opticaltransport/otp/shared/protocol"
)

// Encoder identifiers, carried in the frame header so a receiver can select the
// matching decoder. They are wire values: never renumber them.
const (
	IDBinary    uint8 = 1
	IDGrayscale uint8 = 2
	IDColor8    uint8 = 3
	IDColor16   uint8 = 4
	IDRolling   uint8 = 5
)

// Errors returned by encoders.
var (
	// ErrUnknownEncoder means no encoder is registered under that name or id.
	ErrUnknownEncoder = errors.New("encoding: unknown encoder")

	// ErrUnsupportedBitDepth means the encoder does not offer that bit depth.
	ErrUnsupportedBitDepth = errors.New("encoding: unsupported bit depth")

	// ErrEncoderMismatch means the frame was produced by a different encoder than
	// the one asked to decode it. The receiver uses it to dispatch to the right
	// encoder rather than failing the frame.
	ErrEncoderMismatch = errors.New("encoding: frame was produced by a different encoder")

	// ErrBandDamaged means a rolling-shutter frame had bands that failed their
	// checksums. Inspect it with errors.As to learn which.
	ErrBandDamaged = errors.New("encoding: rolling-shutter bands damaged")
)

// Capacity describes what one frame can carry under a given geometry and depth.
type Capacity struct {
	// BitsPerCell is the modulation's density.
	BitsPerCell int `json:"bits_per_cell"`

	// PayloadCells is how many grid cells carry payload.
	PayloadCells int `json:"payload_cells"`

	// PayloadBytes is the usable payload per frame.
	PayloadBytes int `json:"payload_bytes"`

	// GridCells is the whole grid, and OverheadCells the part spent on fiducials,
	// bands, timing, and descriptors.
	GridCells     int `json:"grid_cells"`
	OverheadCells int `json:"overhead_cells"`

	// Efficiency is payload bits over total grid bits, in 0..1. It is what makes
	// encodings comparable: a colour encoding packs more per cell but spends the
	// same on scaffolding, so its efficiency is strictly better on the same grid.
	Efficiency float64 `json:"efficiency"`
}

// String renders a capacity for logs and the profile UI.
func (c Capacity) String() string {
	return fmt.Sprintf("%d bits/cell, %d payload cells, %d bytes/frame, %.1f%% efficient",
		c.BitsPerCell, c.PayloadCells, c.PayloadBytes, c.Efficiency*100)
}

// Encoder is the plug-in interface every optical encoding implements.
type Encoder interface {
	// ID is the wire identifier written into the frame header.
	ID() uint8

	// Name is the stable configuration name, such as "color16".
	Name() string

	// Description is one line for the profile UI and generated documentation.
	Description() string

	// SupportedBitDepths lists the depths this encoding accepts, ascending.
	SupportedBitDepths() []uint8

	// DefaultBitDepth is the depth used when configuration leaves it unset.
	DefaultBitDepth() uint8

	// Encode renders a frame. It fills in the header fields that describe the
	// encoding and geometry, so callers need not keep them in step by hand.
	Encode(f *protocol.Frame, l protocol.Layout, bitDepth uint8) (*image.RGBA, error)

	// Decode recovers a frame from a captured image, verifying its integrity.
	Decode(img image.Image, opts protocol.LocateOptions) (*protocol.Frame, error)

	// Validate reports whether a frame can be rendered at this geometry and depth.
	Validate(f *protocol.Frame, l protocol.Layout, bitDepth uint8) error

	// EstimateCapacity reports what one frame carries. The sender uses it to pick
	// a chunk size, so that one chunk maps to exactly one frame.
	EstimateCapacity(l protocol.Layout, bitDepth uint8) (Capacity, error)
}

var encoders = registry.New[Encoder]("encoder", ErrUnknownEncoder)

// Register adds an encoder to the registry. It panics on a duplicate id or name,
// because that is a programming error that would otherwise surface as frames
// decoded by the wrong encoder.
func Register(e Encoder) { encoders.Register(e) }

// ByID returns the encoder with that wire identifier.
func ByID(id uint8) (Encoder, error) { return encoders.ByID(id) }

// ByName returns the encoder with that configuration name.
func ByName(name string) (Encoder, error) { return encoders.ByName(name) }

// All returns every registered encoder, ordered by id.
func All() []Encoder { return encoders.All() }

// Names returns every registered encoder name, ordered by id.
func Names() []string { return encoders.Names() }

// Decode dispatches to whichever encoder produced the frame.
//
// A receiver does not know in advance which encoding a sender chose, and it must
// not need to: the header is always binary-modulated, so reading it is possible
// regardless, and the encoder id it carries selects the decoder. That is what
// makes an operator's change of encoding profile transparent to the receiver.
func Decode(img image.Image, opts protocol.LocateOptions) (*protocol.Frame, error) {
	g, err := protocol.Locate(img, opts)
	if err != nil {
		return nil, err
	}
	e, err := ByID(g.Header.EncoderID)
	if err != nil {
		return nil, err
	}
	return decodeAt(e, g, img, opts)
}

// BandDamageError reports which rolling-shutter bands failed their checksums.
type BandDamageError struct {
	Bands      []int
	TotalBands int
}

func (e *BandDamageError) Error() string {
	return fmt.Sprintf("%s: %d of %d bands failed checksum %v",
		ErrBandDamaged.Error(), len(e.Bands), e.TotalBands, e.Bands)
}

func (e *BandDamageError) Unwrap() error { return ErrBandDamaged }
