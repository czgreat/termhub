// The live terminal components, by session id, for the few things the page
// frame, the key bar and the input box need from them (docs/M9 第 5、6 节,
// M11 第 5、6 节).
import type { InputResult } from './session'
export interface TermHandle {
  pasteBatch(text: string): Promise<InputResult>
  inputBatch(text: string): Promise<InputResult>
  /** Input goes out now: the link is attached and the node there. */
  canSend(): boolean
  /** Input is refused for now (a dropped queue): what is sent is lost. */
  refuses(): boolean
  /** Counts the dropped queues: changed, input given meanwhile may be lost. */
  drops(): number
  /** Someone else types into this session: this viewer only watches. */
  readOnly(): boolean
  selection(): string
  /** Pastes text through the terminal's paste path (bracketed when the program asked for it). */
  paste(text: string): void
  /** Sends raw key bytes, with the sticky modifiers applied. */
  input(data: string): void
  /** Uploads files and pastes their paths (M4 第 7 节). */
  files(files: File[]): void
  focus(): void
  /** Whether the program uses application cursor keys (arrows as ESC O x). */
  appCursor(): boolean
}

export const terms = new Map<string, TermHandle>()
