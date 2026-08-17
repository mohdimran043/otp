package shared_test

// The paper channel, measured: print a transfer's frames, scan the sheets, decode what comes back.
//
// docs/PRINT-AND-SCAN.md is written from this test's output and every number in it comes from here. Run it
// with:
//
//	go test ./ -run TestPaperChannelSweep -timeout 30m -v
//
// It is skipped under -short because the full grid takes minutes: each scenario rasterises an A4 page at
// 600dpi, halftones it, and box-integrates it down to the scan resolution, three sheets at a time.
//
// What this can and cannot settle. It is a simulation of print and scan, not a print and a scan — it
// models nearest-neighbour rasterisation, bilevel halftoning with imperfect separation registry, dot gain,
// aperture integration, paper cockle, skew and sensor noise, which are the effects that decide geometry.
// It does not model toner colour accuracy, paper tint, scanner colour profiles, or the specific screening
// a given RIP uses. So a geometry this test fails will fail on paper; one it passes still has to be
// printed once before anybody trusts a stack of four hundred.

import (
	"fmt"
	"image"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/opticaltransport/otp/shared/encoding"
	"github.com/opticaltransport/otp/shared/protocol"
	"github.com/opticaltransport/otp/shared/simulate"
)

// sheetsPerScenario is how many sheets each scenario prints.
//
// Three rather than one, so a rate means something: a single sheet of a single frame can only ever report
// nothing or everything, and the band this is looking for — the geometry that mostly works — is exactly
// the one a single sample cannot describe.
const sheetsPerScenario = 3

// paperArrangement mirrors the printable document's own: portrait, because A4 is taller than it is wide
// and two frames side by side on a portrait page are each bounded by half its width.
func paperArrangement(perPage int) (columns, rows int, ok bool) {
	switch perPage {
	case 1:
		return 1, 1, true
	case 2:
		return 1, 2, true
	case 4:
		return 2, 2, true
	case 6:
		return 2, 3, true
	}
	return 0, 0, false
}

type paperScenario struct {
	grid    int
	encoder string
	depth   uint8
	perPage int
	scanner simulate.Scanner
	stress  float64
}

type paperResult struct {
	paperScenario
	pxPerCell     float64
	printed       int
	located       int
	decoded       int
	bytesPerSheet int
}

// runPaperScenario prints, scans and reads one scenario, and counts only frames whose payload came back
// byte for byte.
//
// Byte-exactness rather than "decoded without error" because the two differ in the way that matters: a
// frame whose header reads and whose body does not is the failure this project has been bitten by
// repeatedly, and counting it as a success would put a geometry in the table that transfers nothing.
func runPaperScenario(s paperScenario) (paperResult, error) {
	lane, err := protocol.NewLayoutQuiet(s.grid, s.grid, 8, protocol.DefaultQuietZone)
	if err != nil {
		return paperResult{}, err
	}
	enc, err := encoding.ByName(s.encoder)
	if err != nil {
		return paperResult{}, err
	}
	capacity, err := enc.EstimateCapacity(lane, s.depth)
	if err != nil {
		return paperResult{}, err
	}

	count := s.perPage * sheetsPerScenario
	txID := uuid.New()
	frames := make([]image.Image, 0, count)
	want := make(map[uint32][]byte, count)
	for i := range count {
		// A payload that fills the frame. A short one would leave most cells carrying padding, and the
		// CRC is over the payload — so a mostly-empty frame is a much easier frame than a real one.
		payload := make([]byte, capacity.PayloadBytes)
		for j := range payload {
			payload[j] = byte((j*31 + i*17) % 251)
		}
		f := protocol.NewFrame(protocol.Header{
			TransmissionID: txID,
			FrameNumber:    uint32(i),
			ChunkNumber:    uint32(i),
			TotalChunks:    uint32(count),
		}, payload)
		img, err := enc.Encode(f, lane, s.depth)
		if err != nil {
			return paperResult{}, err
		}
		frames = append(frames, img)
		want[uint32(i)] = payload
	}

	columns, rows, ok := paperArrangement(s.perPage)
	if !ok {
		return paperResult{}, fmt.Errorf("no arrangement for %d frames a page", s.perPage)
	}

	var sheets []image.Image
	if s.perPage == 1 {
		sheets = frames
	} else {
		tiling := protocol.LaneLayout{
			Lane: lane, Columns: columns, Rows: rows, Gap: protocol.DefaultLaneGapCells,
		}
		for start := 0; start < len(frames); start += s.perPage {
			sheet, err := tiling.Compose(frames[start:min(start+s.perPage, len(frames))])
			if err != nil {
				return paperResult{}, err
			}
			sheets = append(sheets, sheet)
		}
	}

	cellsAcross := columns*(s.grid+2*protocol.DefaultQuietZone) + (columns-1)*protocol.DefaultLaneGapCells
	out := paperResult{
		paperScenario: s,
		printed:       len(frames),
		bytesPerSheet: capacity.PayloadBytes * s.perPage,
	}
	if len(sheets) > 0 {
		b := sheets[0].Bounds()
		out.pxPerCell = simulate.PixelsPerCell(b.Dx(), b.Dy(), cellsAcross, s.scanner)
	}

	opts := protocol.LocateOptions{CellPixelsHint: 8}
	for _, sheet := range sheets {
		scanned := simulate.PrintAndScan(sheet, s.scanner, s.stress)
		found := protocol.LocateAll(scanned, opts, 16)
		out.located += len(found)
		for _, g := range found {
			frame, err := encoding.DecodeAt(g, scanned, opts)
			if err != nil {
				continue
			}
			expected, ok := want[frame.Header.FrameNumber]
			if ok && string(expected) == string(frame.Payload) {
				out.decoded++
			}
		}
	}
	return out, nil
}

// TestPaperChannelSweep measures every combination and prints the tables docs/PRINT-AND-SCAN.md carries.
func TestPaperChannelSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("the paper sweep rasterises hundreds of A4 pages at 600dpi; not for -short")
	}

	grids := []int{64, 96, 128, 192, 256, 384, 512}
	encoders := []struct {
		name  string
		depth uint8
	}{{"binary", 1}, {"color8", 3}}
	perPages := []int{1, 2, 4}

	// 1.0 is the modelled device; 1.4 is a worse print on worse paper read by a tireder scanner. A
	// geometry worth recommending has to survive both, because the absolute figures are the least
	// trustworthy part of this and a recommendation resting on them being exactly right is worthless.
	stresses := []float64{1.0, 1.4}

	var scenarios []paperScenario
	for _, sc := range simulate.Scanners {
		for _, e := range encoders {
			for _, g := range grids {
				for _, pp := range perPages {
					for _, st := range stresses {
						scenarios = append(scenarios, paperScenario{
							grid: g, encoder: e.name, depth: e.depth,
							perPage: pp, scanner: sc, stress: st,
						})
					}
				}
			}
		}
	}

	results := make([]paperResult, len(scenarios))
	errs := make([]error, len(scenarios))
	sem := make(chan struct{}, max(1, runtime.NumCPU()-1))
	var wg sync.WaitGroup
	for i, s := range scenarios {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = runPaperScenario(s)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("scenario %+v: %v", scenarios[i], err)
		}
	}

	index := map[string]paperResult{}
	key := func(scanner, enc string, grid, perPage int, stress float64) string {
		return fmt.Sprintf("%s|%s|%d|%d|%.1f", scanner, enc, grid, perPage, stress)
	}
	for _, r := range results {
		index[key(r.scanner.Name, r.encoder, r.grid, r.perPage, r.stress)] = r
	}

	verdict := func(r paperResult) string {
		switch {
		case r.printed == 0:
			return "—"
		case r.decoded == r.printed:
			return "all"
		case r.decoded == 0 && r.located > 0:
			return fmt.Sprintf("none (located %d)", r.located)
		case r.decoded == 0:
			return "none"
		default:
			return fmt.Sprintf("%d/%d", r.decoded, r.printed)
		}
	}
	safe := func(scanner, enc string, grid, perPage int) string {
		good, ok1 := index[key(scanner, enc, grid, perPage, 1.0)]
		hard, ok2 := index[key(scanner, enc, grid, perPage, 1.4)]
		if !ok1 || !ok2 {
			return "—"
		}
		gok := good.printed > 0 && good.decoded == good.printed
		hok := hard.printed > 0 && hard.decoded == hard.printed
		switch {
		case gok && hok:
			return "SAFE"
		case gok:
			return "tight"
		case good.decoded > 0:
			return "partial"
		default:
			return "fails"
		}
	}

	w := os.Stdout
	fmt.Fprintf(w, "\n=== paper channel, one frame to a page ===\n")
	for _, e := range encoders {
		fmt.Fprintf(w, "\n%s\n", e.name)
		fmt.Fprintf(w, "%-6s %-12s", "grid", "bytes/sheet")
		for _, sc := range simulate.Scanners {
			fmt.Fprintf(w, " | %-13s %-18s %-6s", sc.Name+" px/c", "result", "margin")
		}
		fmt.Fprintln(w)
		for _, g := range grids {
			var bps int
			line := ""
			for _, sc := range simulate.Scanners {
				r := index[key(sc.Name, e.name, g, 1, 1.0)]
				bps = r.bytesPerSheet
				line += fmt.Sprintf(" | %-13.1f %-18s %-6s",
					r.pxPerCell, verdict(r), safe(sc.Name, e.name, g, 1))
			}
			fmt.Fprintf(w, "%-6d %-12d%s\n", g, bps, line)
		}
	}

	fmt.Fprintf(w, "\n=== frames to a page, at the modelled device ===\n")
	for _, e := range encoders {
		fmt.Fprintf(w, "\n%s\n", e.name)
		fmt.Fprintf(w, "%-6s", "grid")
		for _, sc := range simulate.Scanners {
			for _, pp := range perPages {
				fmt.Fprintf(w, " | %-18s", fmt.Sprintf("%s %d-up", sc.Name, pp))
			}
		}
		fmt.Fprintln(w)
		for _, g := range grids {
			fmt.Fprintf(w, "%-6d", g)
			for _, sc := range simulate.Scanners {
				for _, pp := range perPages {
					fmt.Fprintf(w, " | %-18s", verdict(index[key(sc.Name, e.name, g, pp, 1.0)]))
				}
			}
			fmt.Fprintln(w)
		}
	}

	fmt.Fprintf(w, "\n=== densest geometry that survives both stress levels ===\n")
	for _, sc := range simulate.Scanners {
		var best *paperResult
		for i := range results {
			r := results[i]
			if r.scanner.Name != sc.Name || r.stress != 1.0 {
				continue
			}
			if safe(r.scanner.Name, r.encoder, r.grid, r.perPage) != "SAFE" {
				continue
			}
			if best == nil || r.bytesPerSheet > best.bytesPerSheet {
				best = &results[i]
			}
		}
		if best == nil {
			fmt.Fprintf(w, "%-13s nothing passed both\n", sc.Name)
			continue
		}
		fmt.Fprintf(w, "%-13s %-8s grid %-4d %d-up  %7d bytes/sheet  %.1f px/cell\n",
			sc.Name, best.encoder, best.grid, best.perPage, best.bytesPerSheet, best.pxPerCell)
	}

	fmt.Fprintf(w, "\n=== px/cell at the pass/fail boundary (floors from shared/readable: 6 binary, 10 colour) ===\n")
	for _, sc := range simulate.Scanners {
		for _, e := range encoders {
			var pass, fail []float64
			for _, r := range results {
				if r.scanner.Name != sc.Name || r.encoder != e.name || r.stress != 1.0 {
					continue
				}
				if r.printed > 0 && r.decoded == r.printed {
					pass = append(pass, r.pxPerCell)
				}
				if r.decoded == 0 {
					fail = append(fail, r.pxPerCell)
				}
			}
			sort.Float64s(pass)
			sort.Float64s(fail)
			lowest, highest := "—", "—"
			if len(pass) > 0 {
				lowest = fmt.Sprintf("%.1f", pass[0])
			}
			if len(fail) > 0 {
				highest = fmt.Sprintf("%.1f", fail[len(fail)-1])
			}
			fmt.Fprintf(w, "%-13s %-8s lowest fully decoding %-6s highest total failure %s\n",
				sc.Name, e.name, lowest, highest)
		}
	}
}
