package encoding

// The five registered encodings. Each is a codec differing only in its palette
// and its arrangement of the payload region, so there is exactly one
// render-and-read implementation to keep correct.
//
// Bit depth is a wire field, so a frame carries the depth it was rendered at and
// a receiver never has to be configured to match the sender. The depths listed
// here are the ones each encoding accepts; anything else is refused at Encode
// rather than producing a frame nothing can read.
var (
	// Binary is one bit per cell: black or white. It is the widest-margin
	// encoding and the one every fixed band uses, so a frame is always readable
	// down to its header even when the payload modulation is not.
	Binary = &codec{
		id:          IDBinary,
		name:        "binary",
		description: "One bit per cell, black or white. Lowest density, widest noise margin.",
		depths:      []uint8{1},
		defaultDep:  1,
		palette:     func(uint8) Palette { return BinaryPalette },
		planner:     rowMajorPlan,
	}

	// Grayscale trades margin for density on a monochrome sensor, which is what
	// industrial cameras usually are: two or three bits per cell from a grey ramp.
	Grayscale = &codec{
		id:          IDGrayscale,
		name:        "grayscale",
		description: "Two or three bits per cell from a grey ramp, for monochrome sensors.",
		depths:      []uint8{2, 3},
		defaultDep:  2,
		palette:     func(d uint8) Palette { return GrayPalette(int(d)) },
		planner:     rowMajorPlan,
	}

	// Color8 is three bits per cell at the corners of the RGB cube, where no two
	// symbols share a channel value. It triples binary's density at a margin a
	// colour camera holds comfortably.
	Color8 = &codec{
		id:          IDColor8,
		name:        "color8",
		description: "Three bits per cell at the eight corners of the RGB cube.",
		depths:      []uint8{3},
		defaultDep:  3,
		palette:     func(uint8) Palette { return Color8Palette },
		planner:     rowMajorPlan,
	}

	// Color16 is four bits per cell: the densest encoding, and the one that most
	// needs good white balance, since it separates symbols by hue as well as
	// luminance.
	Color16 = &codec{
		id:          IDColor16,
		name:        "color16",
		description: "Four bits per cell from four grey levels and twelve hues.",
		depths:      []uint8{4},
		defaultDep:  4,
		palette:     func(uint8) Palette { return Color16Palette },
		planner:     rowMajorPlan,
	}

	// Rolling is binary modulation interleaved across horizontal bands, each
	// carrying its own checksum. It is the encoding for a rolling-shutter camera:
	// see rolling.go for why interleaving is what makes a torn frame recoverable.
	Rolling = &codec{
		id:          IDRolling,
		name:        "rolling",
		description: "Binary modulation interleaved across bands, each band checksummed.",
		depths:      []uint8{1},
		defaultDep:  1,
		palette:     func(uint8) Palette { return BinaryPalette },
		planner:     rollingPlan,
	}
)

func init() {
	for _, e := range []Encoder{Binary, Grayscale, Color8, Color16, Rolling} {
		Register(e)
	}
}
