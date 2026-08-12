"""The optical symbol classifier.

What this model is for, and what it is deliberately not for.

The decoder reads a cell by averaging a window at its centre and matching the result against eight
palette entries by weighted RGB distance. That is optimal when the only corruption is zero-mean noise
on an isolated sample, and it is measurably not what happens: at four pixels a cell the neighbours
bleed into the centre, so the sampled colour is a mixture whose composition depends on what surrounds
the cell. Nearest-neighbour matching has no way to represent that, however good the palette.

So the model sees a patch spanning one and a half cells rather than a single sample. Its job is to use
the surrounding context to undo the mixing, which is a thing a learned function can do and a distance
metric cannot. It is not an image enhancer, it does not touch geometry, and it does not attempt to
find frames — the existing decoder does all of that better than a network would, because it exploits
the format's own known-value cells.

Architecture notes:

* Small on purpose. This runs once per payload cell, and a 128x128 grid has 12,096 of them per frame.
  A model that costs a millisecond a cell costs twelve seconds a frame and is useless whatever its
  accuracy. Two convolutions and a small head keep the whole frame inside a single batched forward
  pass of a few milliseconds.
* The photometric reference (this frame's measured black and white per channel) is concatenated at the
  head rather than applied to the input. Applying it would limit the model to correcting whatever the
  existing linear normalisation leaves behind; passing it lets the model learn its own mapping,
  including the gamma a linear model provably cannot represent.
* Output is eight logits, so the caller gets a distribution and not just an argmax. That matters
  downstream: the recovery search ranks cells by how unsure the reader was, and a posterior is a far
  better uncertainty estimate than a distance-to-runner-up.
"""

import torch
import torch.nn as nn
import torch.nn.functional as F

PATCH_SIDE = 9
CHANNELS = 3
CLASSES = 8
REFERENCE_FEATURES = 6  # black[3] + white[3]


class SymbolClassifier(nn.Module):
    """Classifies one cell patch into one of eight colour8 symbols."""

    def __init__(self, width: int = 48):
        super().__init__()
        # No pooling. The patch is nine samples across and the cell of interest is the middle third of
        # it; pooling away that resolution would discard exactly the boundary information the model
        # exists to use.
        self.conv1 = nn.Conv2d(CHANNELS, width, kernel_size=3, padding=1)
        self.conv2 = nn.Conv2d(width, width, kernel_size=3, padding=1)
        self.norm1 = nn.BatchNorm2d(width)
        self.norm2 = nn.BatchNorm2d(width)

        # The centre sample is taken out and fed forward separately as well as through the
        # convolutions. It is what the existing decoder uses, so giving the head direct access means
        # the model starts from at least the baseline's information rather than having to rediscover
        # which of the eighty-one samples is the important one.
        self.head = nn.Sequential(
            nn.Linear(width + CHANNELS + REFERENCE_FEATURES, 64),
            nn.ReLU(inplace=True),
            nn.Linear(64, CLASSES),
        )

    def forward(self, patch: torch.Tensor, reference: torch.Tensor) -> torch.Tensor:
        """patch is (N, 3, 9, 9) in 0..1; reference is (N, 6) in 0..1."""
        x = F.relu(self.norm1(self.conv1(patch)), inplace=True)
        x = F.relu(self.norm2(self.conv2(x)), inplace=True)

        # Average over the central 3x3, which is the cell itself rather than its neighbours.
        centre = x[:, :, 3:6, 3:6].mean(dim=(2, 3))
        centre_sample = patch[:, :, 4, 4]

        return self.head(torch.cat([centre, centre_sample, reference], dim=1))


# The colour8 palette, in symbol order: the eight corners of the RGB cube.
# Mirrors shared/encoding/palette.go. If that changes, this must change with it.
PALETTE = torch.tensor(
    [
        [0, 0, 0],
        [255, 0, 0],
        [0, 255, 0],
        [0, 0, 255],
        [0, 255, 255],
        [255, 0, 255],
        [255, 255, 0],
        [255, 255, 255],
    ],
    dtype=torch.float32,
)

# Luminance weights the decoder matches with. A sensor resolves green far better than blue, so an
# unweighted distance throws away real discriminating power.
CHANNEL_WEIGHTS = torch.tensor([0.299, 0.587, 0.114], dtype=torch.float32)


def baseline_predict(patch: torch.Tensor, reference: torch.Tensor) -> torch.Tensor:
    """The decoder's own decision, reproduced so the model can be scored against it on identical data.

    This is the comparison that decides whether the model is worth its cost. Reported accuracy in
    isolation says nothing — the baseline already reads most cells correctly, so a model at 97% could
    be an improvement or a regression depending on where the baseline sits. It is computed here rather
    than exported from Go so that both numbers come from the same tensors in the same run.

    Takes the centre sample, normalises it by the frame's black and white the way encoding's
    photometric reference does, and returns the nearest palette entry under the weighted metric.
    """
    centre = patch[:, :, 4, 4] * 255.0
    black = reference[:, 0:3] * 255.0
    white = reference[:, 3:6] * 255.0

    span = (white - black).clamp(min=1.0)
    normalised = ((centre - black) / span * 255.0).clamp(0.0, 255.0)

    palette = PALETTE.to(patch.device)
    weights = CHANNEL_WEIGHTS.to(patch.device)

    # (N, 8, 3) differences, weighted and summed to (N, 8) distances.
    diff = normalised.unsqueeze(1) - palette.unsqueeze(0)
    distance = (diff * diff * weights).sum(dim=2)
    return distance.argmin(dim=1)
