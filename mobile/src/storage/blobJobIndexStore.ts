// Filesystem-backed job index.
//
// Uses react-native-blob-util's fs rather than AsyncStorage, because the
// uploader already depends on it — one native dependency instead of two, in an
// app whose Android toolchain has not been built once yet.

import ReactNativeBlobUtil from 'react-native-blob-util';

import {JobIndexStore} from './jobIndex';

/**
 * DocumentDir, not CacheDir.
 *
 * Android reclaims cache directories under storage pressure without warning.
 * The index is the only record of which job IDs exist, so putting it somewhere
 * the OS may delete would lose the job history to a low-disk event rather than
 * to anything the user did.
 */
const indexPath = () =>
  `${ReactNativeBlobUtil.fs.dirs.DocumentDir}/dayreel-jobs.json`;

export const blobJobIndexStore: JobIndexStore = {
  async read() {
    const path = indexPath();
    if (!(await ReactNativeBlobUtil.fs.exists(path))) {
      return null;
    }
    try {
      return await ReactNativeBlobUtil.fs.readFile(path, 'utf8');
    } catch {
      // Treated as "no index" rather than an error: parseEntries already
      // tolerates garbage, and the list screen must open regardless.
      return null;
    }
  },

  async write(contents: string) {
    await ReactNativeBlobUtil.fs.writeFile(indexPath(), contents, 'utf8');
  },
};
