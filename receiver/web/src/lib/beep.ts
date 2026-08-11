/**
 * The camera page's tones, synthesised rather than loaded.
 *
 * No audio files: nothing to fetch, nothing for a strict content policy to block, and no second request that
 * could fail on the one page an operator is watching to see whether anything works at all. Three short
 * oscillator notes cover it.
 *
 * Why sound at all — an operator aiming a camera at a monitor cannot watch the screen they are aiming at. The
 * whole point is feedback they can act on without looking.
 */

import type { ScanEvent } from './scanEvents'

let context: AudioContext | null = null

/**
 * ensureContext returns the AudioContext, creating it on first use.
 *
 * Lazily, and never at module load: a browser refuses to start audio without a user gesture, and an
 * AudioContext constructed on page load arrives suspended and stays that way. Created on the first tone
 * instead — which follows the Start button — and resumed each time, because a background tab suspends it again.
 */
function ensureContext(): AudioContext | null {
  if (typeof window === 'undefined') return null

  const Ctor = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
  if (!Ctor) return null

  if (!context) context = new Ctor()
  if (context.state === 'suspended') void context.resume()
  return context
}

/**
 * note plays one tone.
 *
 * The gain envelope is not decoration. A tone that starts and stops at full amplitude clicks, and a page that
 * clicks ten times a second sounds broken rather than informative — so each note ramps up over a few
 * milliseconds and decays away rather than being cut off.
 */
function note(ctx: AudioContext, frequency: number, startAt: number, duration: number, volume: number): void {
  const oscillator = ctx.createOscillator()
  const gain = ctx.createGain()

  oscillator.type = 'sine'
  oscillator.frequency.setValueAtTime(frequency, startAt)

  gain.gain.setValueAtTime(0, startAt)
  gain.gain.linearRampToValueAtTime(volume, startAt + 0.008)
  gain.gain.exponentialRampToValueAtTime(0.0001, startAt + duration)

  oscillator.connect(gain)
  gain.connect(ctx.destination)
  oscillator.start(startAt)
  oscillator.stop(startAt + duration + 0.02)
}

/**
 * play sounds one scanning event.
 *
 * The three are deliberately far apart in pitch, because they have to be told apart by someone not looking at
 * the screen: a chunk is a high blip, the manifest is a lower and longer one, and a verified merge is a rising
 * three-note chime that cannot be mistaken for either.
 */
export function play(event: ScanEvent): void {
  const ctx = ensureContext()
  if (!ctx) return

  const now = ctx.currentTime
  switch (event) {
    case 'chunk':
      note(ctx, 880, now, 0.07, 0.18)
      break
    case 'manifest':
      note(ctx, 440, now, 0.16, 0.2)
      break
    case 'verified':
      note(ctx, 660, now, 0.12, 0.2)
      note(ctx, 880, now + 0.1, 0.12, 0.2)
      note(ctx, 1320, now + 0.2, 0.22, 0.22)
      break
  }
}

/**
 * unlock starts the audio device from inside a user gesture.
 *
 * Called from the Start button so the first real tone is not the one that has to ask permission — on a strict
 * browser that first tone would simply be dropped, and the operator would decide the feature is broken when it
 * was only waiting for a click.
 */
export function unlock(): void {
  ensureContext()
}
