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
} from '@react-native-documents/picker';
import type {NativeStackNavigationProp} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {blobTransport} from '../upload/blobTransport';
import {UploadExpiredError} from '../upload/uploader';
import {uploadVideo} from '../upload/uploadVideo';
import {blobJobIndexStore} from '../storage/blobJobIndexStore';

type HomeScreenProps = {
  navigation: NativeStackNavigationProp<RootStackParamList, 'Home'>;
};

export default function HomeScreen({navigation}: HomeScreenProps) {
  const [progress, setProgress] = useState<number | null>(null);
  const uploading = progress !== null;

  const startUpload = useCallback(
    async (video: {
      fileUri: string;
      filename: string;
      sizeBytes: number;
      contentType: string;
    }) => {
      setProgress(0);
      try {
        const {jobId} = await uploadVideo({
          video,
          transport: blobTransport,
          store: blobJobIndexStore,
          onProgress: setProgress,
        });
        setProgress(null);
        navigation.navigate('Player', {jobId});
      } catch (err) {
        setProgress(null);
        Alert.alert(
          'Upload failed',
          err instanceof UploadExpiredError
            ? 'The upload link expired. Please select the video again.'
            : err instanceof Error
              ? err.message
              : String(err),
        );
      }
    },
    [navigation],
  );

  const handleSelectVideo = useCallback(async () => {
    try {
      const [result] = await pick({type: ['video/*']});
      if (!result) {
        return;
      }

      const filename = result.name ?? 'video.mp4';
      const sizeBytes = result.size ?? 0;
      // POST /jobs rejects an empty filename, a non-positive size or an empty
      // content type with a 400. The picker types all three as nullable, so
      // they are checked here rather than discovered as a failed request.
      if (sizeBytes <= 0) {
        Alert.alert('Cannot upload', 'That file has no readable contents.');
        return;
      }

      // On Android `result.uri` is a `content://` URI, which is not a path the
      // native uploader can slice, and whose permission grant Android can
      // revoke. keepLocalCopy turns it into a real local file.
      //
      // documentDirectory, not cachesDirectory: Android reclaims cache
      // directories under storage pressure, and an upload that has to survive
      // the app being killed must not have its source file vanish underneath
      // it. That is Stage 8A [DECIDE 5], settled here because the picker is
      // where the choice is actually made.
      const [copy] = await keepLocalCopy({
        files: [{uri: result.uri, fileName: filename}],
        destination: 'documentDirectory',
      });

      // keepLocalCopy resolves even when it failed — the failure is reported
      // in `status`, not thrown. Checking it is not optional: a missing check
      // would read `localUri` off an error result and upload nothing.
      if (copy.status !== 'success') {
        Alert.alert('Cannot upload', `Could not read that file: ${copy.copyError}`);
        return;
      }

      Alert.alert(
        'Video selected',
        `${filename}\nSize: ${(sizeBytes / 1024 / 1024).toFixed(1)} MB`,
        [
          {text: 'Cancel', style: 'cancel'},
          {
            text: 'Upload',
            onPress: () => {
              startUpload({
                fileUri: copy.localUri,
                filename,
                sizeBytes,
                contentType: result.type ?? 'video/mp4',
              });
            },
          },
        ],
      );
    } catch (err) {
      if (isErrorWithCode(err) && err.code === errorCodes.OPERATION_CANCELED) {
        return;
      }
      Alert.alert('Could not open the picker', String(err));
    }
  }, [startUpload]);

  return (
    <View style={styles.container}>
      <Text style={styles.title}>DayReel</Text>
      <Text style={styles.subtitle}>Turn your videos into reels</Text>

      <TouchableOpacity
        style={[styles.button, uploading && styles.buttonDisabled]}
        disabled={uploading}
        onPress={handleSelectVideo}>
        <Text style={styles.buttonText}>
          {uploading ? `Uploading ${Math.round(progress * 100)}%` : 'Select Video'}
        </Text>
      </TouchableOpacity>

      {uploading ? (
        <View style={styles.progressBar}>
          <View style={[styles.progressFill, {width: `${progress * 100}%`}]} />
        </View>
      ) : null}

      <TouchableOpacity
        style={styles.secondaryButton}
        disabled={uploading}
        onPress={() => navigation.navigate('JobList')}>
        <Text style={styles.secondaryButtonText}>View Jobs</Text>
      </TouchableOpacity>

      {uploading ? <ActivityIndicator style={styles.spinner} color="#6366f1" /> : null}
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
  progressBar: {
    height: 4,
    width: '100%',
    backgroundColor: '#333333',
    borderRadius: 2,
    marginBottom: 16,
  },
  progressFill: {
    height: '100%',
    borderRadius: 2,
    backgroundColor: '#6366f1',
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
