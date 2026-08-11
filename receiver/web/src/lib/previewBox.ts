/**
 * The shape of the camera preview box.
 *
 * It was hard-coded to 16/9 while the stream can be any shape, and a phone streams portrait. With
 * `object-fit: contain` that is technically honest — the whole frame is visible — and practically useless: a
 * 9:16 stream letterboxed into a 16:9 box becomes a narrow sliver between black bars, too small to judge framing
 * on. An operator aiming a camera at a screen then frames against something they cannot really see.
 *
 * Matching the box to the stream is what makes the preview trustworthy: the same shape as what is actually
 * posted, using the whole width available.
 */

/** The 16:9 fallback, used until the stream reports its size so the box reserves space instead of jumping. */
const fallback = '16 / 9'

/**
 * previewAspect returns a CSS `aspect-ratio` for a stream of the given dimensions.
 *
 * Returns the fallback for anything it cannot use, rather than emitting an invalid value: a malformed
 * aspect-ratio is ignored by the browser, which silently collapses the box and takes the preview with it.
 */
export function previewAspect(width: number, height: number): string {
  if (!Number.isFinite(width) || !Number.isFinite(height)) return fallback
  if (width <= 0 || height <= 0) return fallback
  return `${width} / ${height}`
}
