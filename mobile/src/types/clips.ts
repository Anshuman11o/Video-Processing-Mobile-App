// A video the user picked but has not uploaded yet.
//
// This is a client-only shape — nothing here is on the wire. It exists because
// the flow now has a gap between "picked" and "has a job id": the selection
// screen shows clips that the backend has never heard of, and the upload screen
// turns them into jobs one at a time.

/**
 * One picked clip, already copied into app storage.
 *
 * `fileUri` is deliberately the *local copy*, never the picker's own `uri`. On
 * Android the picker returns a `content://` URI whose permission grant the OS
 * can revoke and which the native uploader cannot slice into parts; the copy
 * made by `keepLocalCopy` is a real file that survives both. A clip only
 * reaches this type after that copy has been confirmed — see HomeScreen, where
 * `keepLocalCopy`'s `status` field is checked, because it resolves on failure
 * rather than throwing.
 *
 * Every field is serialisable: these travel through React Navigation params.
 */
export interface PickedClip {
  /**
   * Stable key for lists and for matching progress back to a row.
   *
   * Not the filename: picking two files with the same name from different
   * folders is ordinary, and duplicate keys make React reuse the wrong row.
   */
  id: string;
  /** A readable `file://` path — the `keepLocalCopy` result, not `result.uri`. */
  fileUri: string;
  filename: string;
  sizeBytes: number;
  contentType: string;
  /**
   * Seconds, once the player has reported it. Undefined until then — the
   * picker does not know a video's duration, only the decoder does, so this is
   * filled in asynchronously and the UI must render without it.
   */
  durationSeconds?: number;
}

/** Total bytes across a selection, for the "N clips, X MB" summary. */
export function totalBytes(clips: PickedClip[]): number {
  return clips.reduce((sum, c) => sum + c.sizeBytes, 0);
}
