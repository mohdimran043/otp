/**
 * What the camera page should make a noise about, decided as a pure function.
 *
 * The logic lives here rather than inside the component so it can be tested without a DOM, an AudioContext, or
 * a camera — the three things that make the feature awkward to exercise and the three things this does not
 * need. The component's job is to poll, call this, and play what comes back.
 */

/** ScanState is the part of the receiver's state that the page makes sounds about. */
export interface ScanState {
  /** The transmission frames are currently arriving for, or null when there is none. */
  transmissionId: string | null
  /** How many non-parity chunks have decoded. Parity chunks are not progress an operator is waiting on. */
  chunksArrived: number
  /** Whether the manifest has arrived — the frame that says what is being assembled. */
  hasManifest: boolean
  /** Whether the merged file has been verified against the hash the sender declared. */
  verified: boolean
}

/** ScanEvent is a tone to play. */
export type ScanEvent = 'manifest' | 'chunk' | 'verified'

/**
 * scanEvents returns the tones to play for a change in state.
 *
 * One beep per *new* chunk, and deliberately not one per decoded frame. The sender redisplays a chunk until its
 * acknowledgement arrives, so at ten frames a second most decodes are of a chunk already held; a beep each time
 * would be a continuous drone that says only "the camera is pointed at something". Beeping when the receiver
 * learns something new means the sound is progress, and silence means the display has nothing left to give —
 * which is the distinction an operator aiming a camera actually needs.
 *
 * One beep however many chunks arrived since the last poll, too. Several can land between two polls, and five
 * tones forty milliseconds apart is heard as one click rather than as five.
 */
export function scanEvents(before: ScanState, after: ScanState): ScanEvent[] {
  // Nothing to say about an absent transmission. This is the idle case, not an error: the page sits here
  // whenever the camera is running and the display has not started.
  if (after.transmissionId === null) return []

  // A different transmission is a different story, so nothing carries over. Without this, moving from a
  // finished transmission's nine chunks to a new one's two reads as a decrease, and the new transmission's
  // chunks would never announce themselves.
  const fresh = before.transmissionId !== after.transmissionId
  const previous: ScanState = fresh
    ? { transmissionId: after.transmissionId, chunksArrived: 0, hasManifest: false, verified: false }
    : before

  const events: ScanEvent[] = []

  // The manifest first: it is what the chunks are for. A transmission is only listed once something has
  // arrived for it, so its first sighting can already carry the manifest — still news to this page, and
  // staying quiet would lose the tone that says the receiver now knows what it is assembling.
  if (after.hasManifest && !previous.hasManifest) events.push('manifest')

  if (after.chunksArrived > previous.chunksArrived) events.push('chunk')

  // Last, because it is the end of the story.
  if (after.verified && !previous.verified) events.push('verified')

  return events
}
