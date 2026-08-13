// The real device transport, built on react-native-blob-util.
//
// Why this library rather than fetch(): a presigned PUT needs the raw bytes of
// one byte range of a file, and fetch() in React Native can only send a string
// or a whole Blob. Reading a part into JS to send it means base64, which
// inflates it 33% and drags every megabyte across the bridge. This library
// streams from disk natively and never lifts the bytes into JavaScript.
//
// Nothing here can be unit-tested without a device — which is exactly why the
// retry, ETag and progress logic lives in uploader.ts instead.

import ReactNativeBlobUtil from 'react-native-blob-util';

import {PartTransport} from './uploader';

/** An HTTP status carried alongside the error, so callers can detect a 403. */
export class HttpError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'HttpError';
    this.status = status;
  }
}

/**
 * `fs.slice` wants a filesystem path, not a URL.
 *
 * The document picker is configured with `copyTo: 'cachesDirectory'`, which
 * yields a `file://` URI pointing at a readable copy. A bare `content://` URI
 * would NOT work here — it is not a path, and on Android it can also be revoked
 * once the picker's permission grant lapses.
 */
function toPath(fileUri: string): string {
  if (fileUri.startsWith('file://')) {
    return decodeURIComponent(fileUri.slice('file://'.length));
  }
  return fileUri;
}

/** Case-insensitive header lookup; servers vary on `ETag` vs `etag`. */
function header(headers: Record<string, string>, name: string): string | undefined {
  const target = name.toLowerCase();
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === target) {
      return v;
    }
  }
  return undefined;
}

export const blobTransport: PartTransport = {
  async putPart({url, fileUri, start, end, onProgress, signal}) {
    const partPath = `${ReactNativeBlobUtil.fs.dirs.CacheDir}/part-${start}-${end}`;

    // Slice the range to its own file. react-native-blob-util can stream a
    // whole file but not a range, so the range has to become a file first.
    await ReactNativeBlobUtil.fs.slice(toPath(fileUri), partPath, start, end);

    try {
      const task = ReactNativeBlobUtil.config({}).fetch(
        'PUT',
        url,
        // Content-Type is deliberately omitted. It is NOT part of the signed
        // headers for these URLs, and sending one that disagrees with what was
        // signed is a classic source of SignatureDoesNotMatch.
        {},
        ReactNativeBlobUtil.wrap(partPath),
      );

      if (signal) {
        signal.addEventListener('abort', () => task.cancel(), {once: true});
      }
      if (onProgress) {
        task.uploadProgress({interval: 100}, written => onProgress(written));
      }

      const response = await task;
      const status = response.info().status;
      if (status < 200 || status >= 300) {
        throw new HttpError(status, `S3 returned ${status} for part upload`);
      }

      const etag = header(response.info().headers ?? {}, 'etag');
      if (!etag) {
        // Without an ETag the part cannot be named in CompleteMultipartUpload,
        // so a 200 with no ETag is a failure, not a success.
        throw new Error('S3 accepted the part but returned no ETag header');
      }
      return etag;
    } finally {
      // Best-effort. A leaked slice wastes cache space; failing the upload over
      // it would be worse.
      await ReactNativeBlobUtil.fs.unlink(partPath).catch(() => undefined);
    }
  },
};
