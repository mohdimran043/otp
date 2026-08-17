# Transfer speed

What this platform actually moves, and which setting decides it.

## Transfer speed

Throughput is **bytes per frame × frames per second**. Bigger grids carry more per frame; the panel sets
how many frames a second. The grid is chosen once per transfer, in the New Transfer form; the tables below
describe what each grid carries, whichever transfer picks it.

**Capacity per frame, measured** — `color16` is four bits per cell, `color8` is three:

| Grid | Frame size<br>at 4 px cells | `color8` | `color16` | Best for |
|---|---|---|---|---|
| **256×256** | 1 040 px | 22 669 B | **30 226 B** | 1080p panel, any camera |
| **384×384** | 1 552 px | 52 716 B | **70 288 B** | 4K panel — **the measured sweet spot** |
| **512×512** | 2 064 px | 94 860 B | **126 480 B** | 4K panel + 4K camera, for the largest files |

**What that means in throughput**, `color16`:

| Grid | 25 fps | 40 fps | 60 fps | 1 GB takes |
|---|---|---|---|---|
| 256×256 | 738 KB/s | 1.15 MB/s | 1.73 MB/s | 10 min at 60 fps |
| **384×384** | **1.68 MB/s** | 2.68 MB/s | 4.02 MB/s | 4 min at 60 fps |
| **512×512** | 3.02 MB/s | 4.82 MB/s | **7.24 MB/s** | **2.4 min at 60 fps** |

**Measured end to end**, 8 MB of incompressible data, verified and delivered:

| Configuration | Offered | **Achieved** | Losses |
|---|---|---|---|
| 256×256 @ 8 px, 40 fps | 1.15 MB/s | 730 KiB/s | none |
| **384×384 @ 5 px, 25 fps** | 1.68 MB/s | **1.45 MB/s** | none |

**Prefer fewer, larger frames.** Twice the throughput at less than two-thirds the frame rate — every frame
costs the same fixed overhead whatever it carries, so larger frames amortise it.

### One transfer, all the arithmetic

An 8.6 MB file at `512×512` / `color8` / `zstd` / Reed–Solomon 32+8 — the defaults, on the largest grid that
a 4K panel and a 4K camera can carry:

| | | |
|---|---|---|
| The file | **8 612 637 B** | as uploaded |
| Compressed | **6 232 607 B** | zstd, 72.4% of the original — 27.6% never has to cross |
| Per frame | **94 860 B** | 512×512 at three bits a cell, from the table above |
| Data chunks | **66** | `⌈6 232 607 ÷ 94 860⌉` — 65 full, and a last one of 66 707 B |
| FEC blocks | **3** | `⌈66 ÷ 32⌉` — coding runs per block, not over the whole file |
| Parity chunks | **24** | 3 blocks × 8 parity each |
| **Frames displayed** | **91** | 90 chunks + 1 manifest |

The parity figure is the one that surprises people: 24 for 66, not the 16 or 17 that 32+8 suggests. Coding
is per block, and the last block holds only the 2 shards that were left over — yet it still gets a full 8
parity shards, 400% redundancy on the tail. That is not waste to engineer away. Shards near the end of a
file are the ones a transfer that gets cut short loses, and a short final block is exactly the case where
losing two chunks would otherwise be unrecoverable.

At 25 fps one pass over all 91 frames takes 3.6 seconds. A pass is not the transfer, though — each chunk
keeps being redisplayed until its acknowledgement arrives, so what the panel actually spends its time on is
whatever the receiver has not yet confirmed.

### The ceiling: the receiver, not the display

Decoding costs **85–115 KB/s per CPU core**, and that barely changes with geometry. Your core count sets
the limit:

```
frames per second  =  cores × 115 000 ÷ bytes per frame
```

| Cores | Sustainable | Reality |
|---|---|---|
| 4 | 460 KB/s | A laptop |
| 19 | **2.2 MB/s** | The 20-core workstation everything here was measured on |
| 64 | 7.2 MB/s | No ordinary machine — **use several channels instead** |

**7 MB/s is a multi-channel target.** Four screen–camera pairs at 1.8 MB/s each, carrying disjoint ranges
of chunks of one file. The foundations exist — chunks are addressed by number, the database claims work
atomically, acknowledgements are per chunk — but the range-claiming display loop is **designed and not yet
built** ([overview](docs/optical-transport-overview.pdf), §8).
