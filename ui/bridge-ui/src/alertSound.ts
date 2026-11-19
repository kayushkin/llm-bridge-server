import { ALERT_SOUNDS } from './awaitingReply'

let context: AudioContext | null = null

/** Plays one of `ALERT_SOUNDS` at a volume from 0 to 1. A browser keeps audio
 *  locked until the page has had a click or a key press; until then this
 *  throws, and the caller shows why no sound played. */
export async function playAlertSound(name: string, volume: number): Promise<void> {
  const sound = ALERT_SOUNDS.find(candidate => candidate.name === name)
  if (!sound) throw new Error(`no alert sound named ${name}`)
  context ??= new AudioContext()
  if (context.state === 'suspended') await context.resume()
  if (context.state !== 'running') throw new Error('the browser has not allowed sound on this page yet; click anywhere on it once')
  const start = context.currentTime + 0.02
  for (const note of sound.notes) {
    const oscillator = context.createOscillator()
    const gain = context.createGain()
    oscillator.type = 'sine'
    oscillator.frequency.value = note.frequency
    gain.gain.setValueAtTime(0, start + note.start)
    gain.gain.linearRampToValueAtTime(0.4 * volume, start + note.start + 0.01)
    gain.gain.exponentialRampToValueAtTime(0.0001, start + note.start + note.length)
    oscillator.connect(gain).connect(context.destination)
    oscillator.start(start + note.start)
    oscillator.stop(start + note.start + note.length + 0.05)
  }
}
