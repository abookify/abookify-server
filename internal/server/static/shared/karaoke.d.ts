// Type declarations for karaoke.js so TypeScript consumers (the mobile
// app) get full intellisense + type checking without converting the
// shared module to TS (which would force a build step on the web side).

export interface SyncWord {
  s: number;
  e: number;
  w: string;
}

export interface HighlightWindow {
  readStart: number;
  readEnd: number;
}

export interface AudioSource {
  file: string;
  offsetSecs: number;
  durationSecs: number;
}

export interface FileTimePosition {
  fileIdx: number;
  localSecs: number;
}

export interface ChapterRecord {
  startSec?: number;
  endSec?: number;
  start_sec?: number;
  end_sec?: number;
  title?: string;
  index?: number;
}

export const SYNC_LEAD_SECS: number;
export const WINDOW_BEHIND: number;
export const WINDOW_AHEAD: number;

export function findActiveWord(words: SyncWord[] | null | undefined, timeSecs: number): number;
export function highlightWindow(activeIdx: number, totalWords: number, behind?: number, ahead?: number): HighlightWindow;
export function fileToBookTime(localSecs: number, offsetSecs: number): number;
export function bookToFileTime(bookSecs: number, sources: AudioSource[] | null | undefined): FileTimePosition;
export function chapterAtTime<T extends ChapterRecord>(chapters: T[] | null | undefined, timeSecs: number): T | null;
