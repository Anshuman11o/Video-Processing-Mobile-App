import React, {useCallback, useState} from 'react';
import {
  View,
  Text,
  TouchableOpacity,
  StyleSheet,
  Alert,
  ActivityIndicator,
} from 'react-native';
import {
  errorCodes,
  isErrorWithCode,
  keepLocalCopy,
  pick,
  type DocumentPickerResponse,
} from '@react-native-documents/picker';
import type {NativeStackNavigationProp} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import type {PickedClip} from '../types/clips';

type HomeScreenProps = {
  navigation: NativeStackNavigationProp<RootStackParamList, 'Home'>;
};

/** A file the picker returned that cannot be uploaded, and why. */
interface RejectedClip {
  filename: string;
  reason: string;
}

/**
 * Turn the picker's results into clips, copying each one into app storage.
 *
 * Exported for the sake of being readable in one piece rather than for reuse:
 * this is where four separate failure modes are handled, and burying them in a
 * button handler is how the single-select version ended up with the only
 * `status` check in the app.
 *
 * Every file is copied in a single `keepLocalCopy` call, whose result array is
 * positional — response `i` describes file `i`. That is the only thing tying a
 * copy back to its source, so the two arrays are zipped by index and a file
 * with no corresponding response is rejected rather than assumed good.
 */
export async function toClips(
  picked: DocumentPickerResponse[],
): Promise<{clips: PickedClip[]; rejected: RejectedClip[]}> {
  const clips: PickedClip[] = [];
  const rejected: RejectedClip[] = [];

  // `POST /jobs` rejects an empty filename, a non-positive size or an empty
  // content type with a 400. The picker types all three as nullable, so they
  // are checked here rather than discovered as a failed request — and with
  // several files at once, one unreadable file must not sink the others.
  const usable = picked.filter(result => {
    if ((result.size ?? 0) <= 0) {
      rejected.push({
        filename: result.name ?? 'unnamed file',
        reason: 'no readable contents',
      });
      return false;
    }
    return true;
  });

  if (usable.length === 0) {
    return {clips, rejected};
  }

  // On Android `result.uri` is a `content://` URI, which is not a path the
  // native uploader can slice, and whose permission grant Android can revoke.
  // keepLocalCopy turns it into a real local file.
  //
  // documentDirectory, not cachesDirectory: Android reclaims cache directories
  // under storage pressure, and an upload that has to survive the app being
  // killed must not have its source file vanish underneath it. That is Stage
  // 8A [DECIDE 5], settled here because the picker is where the choice is
  // actually made.
  const files = usable.map(result => ({
    uri: result.uri,
    fileName: result.name ?? 'video.mp4',
  }));
  const copies = await keepLocalCopy({
    // `usable.length > 0` is checked above; the picker's own option type wants
    // a non-empty tuple, which `map` cannot produce on its own.
    files: files as [(typeof files)[0], ...typeof files],
    destination: 'documentDirectory',
  });

  usable.forEach((result, i) => {
    const filename = result.name ?? 'video.mp4';
    const copy = copies[i];

    // keepLocalCopy resolves even when it failed — the failure is reported in
    // `status`, not thrown. Checking it is not optional: a missing check would
    // read `localUri` off an error result and carry a broken path forward
    // silently, one clip at a time, all the way to a failed upload.
    if (!copy) {
      rejected.push({filename, reason: 'no copy result'});
      return;
    }
    if (copy.status !== 'success') {
      rejected.push({filename, reason: copy.copyError});
      return;
    }

    clips.push({
      id: `${i}-${result.uri}`,
      fileUri: copy.localUri,
      filename,
      sizeBytes: result.size ?? 0,
      contentType: result.type ?? 'video/mp4',
    });
  });

  return {clips, rejected};
}

export default function HomeScreen({navigation}: HomeScreenProps) {
  const [preparing, setPreparing] = useState(false);

  const handleSelectVideos = useCallback(async () => {
    try {
      const picked = await pick({
        type: ['video/*'],
        // The flow now starts with a selection, not with a single file: the
        // point of the app is turning a set of clips into one captioned
        // stream, and picking them one at a time made that four round trips
        // through the system picker.
        allowMultiSelection: true,
      });
      if (picked.length === 0) {
        return;
      }

      setPreparing(true);
      const {clips, rejected} = await toClips(picked);
      setPreparing(false);

      if (rejected.length > 0) {
        Alert.alert(
          clips.length > 0 ? 'Some clips were skipped' : 'Cannot use those files',
          rejected.map(r => `${r.filename}: ${r.reason}`).join('\n'),
        );
      }
      if (clips.length === 0) {
        return;
      }

      // Straight to the selection screen, not to an upload. Uploading on pick
      // is what the old single-select flow did, and it left no moment in which
      // the originals could be seen — which is the whole "before" half of what
      // this app claims to do.
      navigation.navigate('Selection', {clips});
    } catch (err) {
      setPreparing(false);
      if (isErrorWithCode(err) && err.code === errorCodes.OPERATION_CANCELED) {
        return;
      }
      Alert.alert('Could not open the picker', String(err));
    }
  }, [navigation]);

  return (
    <View style={styles.container}>
      <Text style={styles.title}>CaptionClips</Text>
      <Text style={styles.subtitle}>Raw video clip to captioned clips.</Text>

      <TouchableOpacity
        style={[styles.button, preparing && styles.buttonDisabled]}
        disabled={preparing}
        onPress={handleSelectVideos}>
        <Text style={styles.buttonText}>
          {preparing ? 'Preparing clips…' : 'Select Videos'}
        </Text>
      </TouchableOpacity>

      <TouchableOpacity
        style={styles.secondaryButton}
        disabled={preparing}
        onPress={() => navigation.navigate('JobList')}>
        <Text style={styles.secondaryButtonText}>View Jobs</Text>
      </TouchableOpacity>

      {preparing ? (
        <ActivityIndicator style={styles.spinner} color="#6366f1" />
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    justifyContent: 'center',
    alignItems: 'center',
    backgroundColor: '#0f0f0f',
    padding: 24,
  },
  title: {
    fontSize: 36,
    fontWeight: '700',
    color: '#ffffff',
    marginBottom: 8,
  },
  subtitle: {
    fontSize: 16,
    color: '#888888',
    marginBottom: 48,
  },
  button: {
    backgroundColor: '#6366f1',
    paddingHorizontal: 32,
    paddingVertical: 16,
    borderRadius: 12,
    marginBottom: 16,
    width: '100%',
    alignItems: 'center',
  },
  buttonDisabled: {
    opacity: 0.6,
  },
  buttonText: {
    color: '#ffffff',
    fontSize: 18,
    fontWeight: '600',
  },
  spinner: {
    marginTop: 24,
  },
  secondaryButton: {
    paddingHorizontal: 32,
    paddingVertical: 16,
    borderRadius: 12,
    borderWidth: 1,
    borderColor: '#333333',
    width: '100%',
    alignItems: 'center',
  },
  secondaryButtonText: {
    color: '#888888',
    fontSize: 16,
  },
});
