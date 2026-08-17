# Print and scan

The third channel. A transfer's frames are exported as a PDF, the sheets are printed, and the printed
sheets are photographed or scanned back in. No display, no camera aimed at a panel, no clock — the
frames sit still on paper until somebody reads them.

That makes paper the slowest channel by an enormous margin and the only one that survives having no
power at either end. It is also the one where a wrong geometry is most expensive, because the cost of
finding out is a stack of four hundred wasted sheets.

Every number here comes from `TestPaperChannelSweep` in `shared/paper_channel_test.go`:

```bash
cd shared && go test ./ -run TestPaperChannelSweep -timeout 25m -v
```

It takes about four minutes. Each scenario rasterises an A4 page at 600 dpi, halftones it, and
box-integrates it down to the scanner's resolution, three sheets at a time.


## The short answer

**One frame on one A4 sheet carries about 52 KB at the outside, and about 22 KB with margin to spare.**

| If you want | Use | Per sheet |
|---|---|---|
| The most a sheet will hold, on a 600 dpi flatbed | `color8`, grid 384, one frame per sheet | **52,716 bytes** |
| A geometry that reads on anything, including a phone photo | `color8`, grid 256, one frame per sheet | **22,669 bytes** |
| Black and white only, e.g. a laser printer and a plain copier | `binary`, grid 384, one frame per sheet | **17,572 bytes** |

Grid 512 is where paper stops. At `binary` it holds 31,620 bytes a sheet and only reads on a 600 dpi
flatbed with no margin; at `color8` it holds 94,860 and reads on nothing. The grid is located in both
cases — the corner markers are found — and then no cell can be told from its neighbour.


## One frame to a page

`px/c` is pixels per cell as scanned. The verdict is measured at two stress levels, a nominal print and
a deliberately bad one (more dot gain, more skew, more cockle, more sensor noise):

- **SAFE** — every frame decoded at both stress levels.
- **tight** — every frame decoded at the nominal one, and not at the bad one.
- **fails** — nothing decoded at either.

### binary

| grid | bytes/sheet | flatbed 600 px/c | | flatbed 300 px/c | | phone 12 MP px/c | |
|---|---|---|---|---|---|---|---|
| 64 | 7 | 64.1 | SAFE | 32.1 | SAFE | 34.2 | SAFE |
| 96 | 634 | 43.6 | SAFE | 21.8 | SAFE | 23.3 | SAFE |
| 128 | 1,512 | 33.0 | SAFE | 16.5 | SAFE | 17.6 | SAFE |
| 192 | 4,061 | 22.2 | SAFE | 11.1 | SAFE | 11.9 | SAFE |
| 256 | 7,556 | 16.8 | SAFE | 8.4 | SAFE | 8.9 | SAFE |
| 384 | 17,572 | 11.2 | SAFE | 5.6 | SAFE | 6.0 | tight |
| 512 | 31,620 | 8.5 | tight | 4.2 | fails | 4.5 | fails |

### color8

| grid | bytes/sheet | flatbed 600 px/c | | flatbed 300 px/c | | phone 12 MP px/c | |
|---|---|---|---|---|---|---|---|
| 64 | 23 | 64.1 | SAFE | 32.1 | SAFE | 34.2 | SAFE |
| 96 | 1,903 | 43.6 | SAFE | 21.8 | SAFE | 23.3 | SAFE |
| 128 | 4,536 | 33.0 | SAFE | 16.5 | SAFE | 17.6 | SAFE |
| 192 | 12,183 | 22.2 | SAFE | 11.1 | SAFE | 11.9 | SAFE |
| 256 | 22,669 | 16.8 | SAFE | 8.4 | SAFE | 8.9 | SAFE |
| 384 | 52,716 | 11.2 | SAFE | 5.6 | tight | 6.0 | tight |
| 512 | 94,860 | 8.5 | fails | 4.2 | fails | 4.5 | fails |

Colour is worth roughly three times binary at the same grid, which is what three bits a cell against one
should give. It costs nothing in geometry — the same grid is the same cell size on the page — so on a
colour printer being read by a colour scanner there is no reason to print binary.


## More than one frame to a page

Tiling puts two or four frames on a sheet. It multiplies what a sheet holds and divides the cell size,
and on paper the second effect wins earlier than it does on a screen.

**These two tables are the nominal print only** — unlike the tables above, they carry no margin column,
so an `all` here means "decoded under a good print", not "decoded under a bad one too". For a verdict
that survives a bad print, use the densest-surviving table further down.

### binary

| grid | 600 1‑up | 600 2‑up | 600 4‑up | 300 1‑up | 300 2‑up | 300 4‑up | phone 1‑up | phone 2‑up | phone 4‑up |
|---|---|---|---|---|---|---|---|---|---|
| 64 | all | all | all | all | all | all | all | all | all |
| 96 | all | all | all | all | all | all | all | all | all |
| 128 | all | all | all | all | all | all | all | all | all |
| 192 | all | all | all | all | all | all | all | all | all |
| 256 | all | all | all | all | all | none | all | all | none |
| 384 | all | 3/6 | none | all | none | none | all | none | none |
| 512 | all | none | none | none | none | none | none | none | none |

### color8

| grid | 600 1‑up | 600 2‑up | 600 4‑up | 300 1‑up | 300 2‑up | 300 4‑up | phone 1‑up | phone 2‑up | phone 4‑up |
|---|---|---|---|---|---|---|---|---|---|
| 64 | all | all | all | all | all | all | all | all | all |
| 96 | all | all | all | all | all | all | all | all | all |
| 128 | all | all | all | all | all | all | all | all | all |
| 192 | all | all | all | all | all | 10/12 | all | all | 5/12 |
| 256 | all | all | 1/12 | all | all | none | all | all | none |
| 384 | all | none | none | all | none | none | all | none | none |
| 512 | none | none | none | none | none | none | none | none | none |

Read the partial results as warnings rather than as capacity. `10/12` is a geometry that will lose a
frame every few sheets, and on paper a lost frame means finding it in the stack and rescanning it.

Tiling is worth it below grid 256 and not worth it above, and it never beats a single larger frame on
capacity: four grid‑192 colour frames are 48,732 bytes against the 52,716 one grid‑384 frame carries.
What tiling buys is a *bigger cell* for a similar payload — 22.2 px a cell at grid 192 against 11.2 at
grid 384 on a 600 dpi scan — which is the whole argument for it on paper, where the failure mode is
cells too small to tell apart. It is the same trade the display channel makes, arrived at from the
opposite direction.

Below 300 dpi that trade is what the numbers actually recommend: on both the 300 dpi flatbed and the
phone, the densest geometry that survives a bad print is two grid‑192 frames to a sheet.


## The densest thing that survives

Computed by the test rather than chosen: the highest bytes per sheet that decodes completely at **both**
stress levels.

| Scanner | Encoding | Grid | Layout | Per sheet | px/cell |
|---|---|---|---|---|---|
| flatbed 600 dpi | color8 | 384 | 1‑up | 52,716 | 11.2 |
| flatbed 300 dpi | color8 | 192 | 2‑up | 24,366 | 7.8 |
| phone, 12 MP | color8 | 192 | 2‑up | 24,366 | 8.3 |

A phone photograph is worth about as much as a 300 dpi flatbed scan, which is the useful surprise in
this table. If the sheets are being read back by whoever is holding them, they do not need a scanner.


## Where the boundary actually is

The floors in `shared/readable` — 6 px a cell binary, 10 colour — were measured against a lit panel.
Paper is a different channel, and the boundary sits in a different place:

| Scanner | Encoding | Lowest that fully decodes | Highest that fails outright |
|---|---|---|---|
| flatbed 600 | binary | 8.3 | 6.0 |
| flatbed 600 | color8 | 11.0 | 8.5 |
| flatbed 300 | binary | 5.5 | 4.2 |
| flatbed 300 | color8 | 5.6 | 4.2 |
| phone 12 MP | binary | 5.8 | 4.5 |
| phone 12 MP | color8 | 6.0 | 4.5 |

The 600 dpi flatbed needs *more* pixels per cell than the 300 dpi one to read the same frame. That is
not a mistake in the table. The likely cause — an inference from the model rather than something the
test isolates — is the halftone: at 600 dpi the scan resolves the individual printed dots, so a cell
that should read as one tone arrives as a bilevel texture, while at 300 dpi the sensor's aperture
averages several dots back into the grey that was intended. **Scanning at a higher resolution than the
print was screened at can make decoding worse, not better.**


## What this settles, and what it does not

The model covers the effects that decide whether a geometry survives: nearest-neighbour rasterisation
(the printable PDF omits `/Interpolate` deliberately, so readers must not smooth cells into their
neighbours), bilevel halftoning with imperfect separation registry, dot gain, aperture integration,
paper cockle, skew, and sensor noise.

It does not cover toner colour accuracy, paper tint, scanner colour profiles, or the particular
screening a given RIP uses.

So: a geometry this test fails will fail on paper. A geometry it passes still deserves one printed sheet
before anybody commits to four hundred.


## One thing this found

The sweep was written to answer a capacity question and turned up a decoder bug on the way. `LocateAll`
lost a lane on every *stacked* tiling above grid 128.

A stacked pair's outer corners describe an almost-square region once the gap between lanes is counted —
1480 × 1616 at grid 192, a ratio of 1.09 — which passes the 1.6 shape tolerance. The grid descriptor read
at its top-left corner is a real lane's real descriptor, so the checksum passes too. The oversized quad
was then accepted, consumed the corners of the lane it had borrowed them from, and both frames were lost.

The timing pattern is what distinguishes them, and `LocateAll` now checks it. See
`TestLocateAllFindsEveryLaneOnAStackedTiling` in `shared/protocol`.

This is the argument for measuring a channel instead of reasoning about it. The bug was invisible on a
screen, invisible at the grid the printable export's own test happened to use, and cost two frames out
of every stacked sheet in silence.


---

See also: [the channel](channel.md) for the other two ways frames cross the gap, and
[performance](performance.md) for what any of this moves per second when there is a display involved.
