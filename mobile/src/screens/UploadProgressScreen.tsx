// The upload phase: every clip at once.
//
// M6 lives here in its live form: megabytes uploaded against total megabytes,
// the part count, and throughput. Everything on this screen is derived from
// what the uploader reports, not from `GET /jobs/{id}` — the API has nothing to
// say about bytes in flight, and the cached job status would be several seconds
// stale even if it did.
//
// Rows advance together because `runUploadBatch` launches them together. A row
// that reads "queued" after the batch has started is not this screen waiting
// its turn — it is WorkManager holding that request, which it is entitled to
// do and which is worth showing rather than hiding behind a stalled bar.

import React, {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {
  View,
  Text,
  FlatList,
  StyleSheet,
  ActivityIndicator,
  TouchableOpacity,
  Platform,
} from 'react-native';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import type {PickedClip} from '../types/clips';
import {
  type ClipUploadState,
  backgroundClipUploader,
  batchFinished,
  batchFraction,
  foregroundClipUploader,
  runUploadBatch,
  uploadedCount,
} from '../upload/uploadBatch';
import {isBackgroundUploadAvailable} from '../upload/nativeUploader';
import {blobJobIndexStore} from '../storage/blobJobIndexStore';
import {uploadProgressMetrics} from '../metrics/jobMetrics';
import {useTicker} from '../hooks/useTicker';

/**
 * Slows the native worker so an upload can actually be watched.
 *
 * [DECIDE 6]: against loopback a multi-part upload finishes in well under a
 * second, which leaves no window in which to kill the app and observe that the
 * upload survived — the stage's original claim — and now also no window in
 * which M6 is ever on screen. `__DEV__` gates it so a release build never
 * carries it.
 *
 * Moved here from HomeScreen along with the upload itself; the value and its
 * reasoning are unchanged.
 */
const DEBUG_PART_DELAY_MS = __DEV__ ? 3000 : 0;

type UploadProgressScreenProps = NativeStackScreenProps<
  RootStackParamList,
  'UploadProgress'
>;

export default function UploadProgressScreen({
  route,
  navigation,
}: UploadProgressScreenProps) {
  const {clips} = route.params;
  const [states, setStates] = useState<ClipUploadState[]>([]);
  const [fatal, setFatal] = useState<string | null>(null);

  // Ticks the throughput figures. The uploader reports progress far more often
  // than once a second, so this is only here to keep the numbers moving during
  // the gaps between a slow part's callbacks.
  const now = useTicker(500, !batchFinished(states));

  // The batch is started exactly once, by a ref rather than by an effect
  // dependency: React runs effects twice in development, and starting the same
  // upload twice would create two jobs per clip.
  const started = useRef(false);
  useEffect(() => {
    if (started.current) {
      return;
    }
    started.current = true;

    // Prefer the background path: it is the one that survives the app being
    // killed. Falling back on Android means the native module did not
    // register, which looks identical to success from here, so it is announced
    // rather than assumed.
    const uploader = isBackgroundUploadAvailable()
      ? backgroundClipUploader({
          store: blobJobIndexStore,
          debugPartDelayMs: DEBUG_PART_DELAY_MS,
        })
      : foregroundClipUploader({store: blobJobIndexStore});

    if (__DEV__ && Platform.OS === 'android' && !isBackgroundUploadAvailable()) {
      console.warn(
        'Falling back to the foreground uploader: these uploads will NOT ' +
          'survive the app being killed.',
      );
    }

    runUploadBatch({clips, uploader, onState: setStates}).catch(err =>
      setFatal(err instanceof Error ? err.message : String(err)),
    );
  }, [clips]);

  const done = uploadedCount(states);
  const finished = batchFinished(states);
  const overall = batchFraction(clips, states);
  const failures = states.filter(s => s.phase === 'failed');

  const goToJobs = useCallback(() => {
    // `replace`, not `navigate`: backing out of the job list should return to
    // Home, not to a finished upload screen that would restart the batch.
    navigation.replace('JobList');
  }, [navigation]);

  // Straight through to the jobs list once the bytes are up — the interesting
  // part is now the pipeline, and it has already started.
  useEffect(() => {
    if (finished && failures.length === 0) {
      const timer = setTimeout(goToJobs, 600);
      return () => clearTimeout(timer);
    }
  }, [finished, failures.length, goToJobs]);

  const byId = useMemo(
    () => new Map(states.map(s => [s.clipId, s])),
    [states],
  );

  return (
    <View style={styles.container}>
      <View style={styles.header}>
        <Text style={styles.counter}>
          {done} of {clips.length} uploaded
        </Text>
        <View style={styles.overallBar}>
          <View style={[styles.overallFill, {width: `${overall * 100}%`}]} />
        </View>
        <Text style={styles.headerHint}>
          {finished
            ? failures.length > 0
              ? `${failures.length} failed`
              : 'All clips uploaded'
            : `Uploading ${clips.length} clip${
                clips.length === 1 ? '' : 's'
              } at once.`}
        </Text>
      </View>

      <FlatList
        data={clips}
        keyExtractor={clip => clip.id}
        renderItem={({item}) => (
          <ClipProgressRow
            clip={item}
            state={byId.get(item.id)}
            nowMs={now}
          />
        )}
        contentContainerStyle={styles.list}
      />

      {fatal ? <Text style={styles.error}>{fatal}</Text> : null}

      {finished ? (
        <View style={styles.footer}>
          <TouchableOpacity style={styles.cta} onPress={goToJobs}>
            <Text style={styles.ctaText}>View jobs</Text>
          </TouchableOpacity>
        </View>
      ) : null}
    </View>
  );
}

/** One clip's row: the M6 numbers while it is in flight. */
function ClipProgressRow({
  clip,
  state,
  nowMs,
}: {
  clip: PickedClip;
  state: ClipUploadState | undefined;
  nowMs: number;
}) {
  const phase = state?.phase ?? 'queued';
  const fraction = state?.fraction ?? 0;

  // Only counted while the clip is actually transferring; a queued clip has no
  // elapsed time, and a finished one should stop moving.
  const elapsedMs =
    state?.startedAtMs === undefined
      ? undefined
      : (state.finishedAtMs ?? nowMs) - state.startedAtMs;

  const metrics = uploadProgressMetrics({
    fraction,
    totalBytes: clip.sizeBytes,
    uploadedParts: state?.uploadedParts,
    totalParts: state?.totalParts,
    elapsedMs,
  });

  const barColor = phase === 'failed' ? '#ef4444' : '#6366f1';

  return (
    <View style={styles.row}>
      <View style={styles.rowHeader}>
        <Text style={styles.filename} numberOfLines={1}>
          {clip.filename}
        </Text>
        <PhaseBadge phase={phase} />
      </View>

      <View style={styles.bar}>
        <View
          style={[
            styles.barFill,
            {width: `${fraction * 100}%`, backgroundColor: barColor},
          ]}
        />
      </View>

      <View style={styles.metaRow}>
        {/* MB uploaded / total MB is the required figure and always shows,
            including for a clip WorkManager has not started — "0.0 MB / 14.3
            MB" says what is happening, where "waiting" alone does not. Parts
            and throughput are additions that depend on what the uploader can
            see. */}
        <Text style={styles.meta}>{metrics.bytesLabel}</Text>
        {metrics.partsLabel ? (
          <Text style={styles.metaDim}>parts {metrics.partsLabel}</Text>
        ) : null}
        {metrics.throughputLabel && phase === 'uploading' ? (
          <Text style={styles.metaDim}>{metrics.throughputLabel}</Text>
        ) : null}
      </View>

      {phase === 'queued' ? (
        <Text style={styles.metaWaiting}>waiting for the scheduler</Text>
      ) : null}

      {state?.error ? <Text style={styles.error}>{state.error}</Text> : null}
    </View>
  );
}

function PhaseBadge({phase}: {phase: ClipUploadState['phase']}) {
  if (phase === 'uploading') {
    return <ActivityIndicator size="small" color="#6366f1" />;
  }
  const color =
    phase === 'done' ? '#22c55e' : phase === 'failed' ? '#ef4444' : '#6b7280';
  const label = phase === 'done' ? 'UPLOADED' : phase === 'failed' ? 'FAILED' : 'QUEUED';
  return (
    <View style={[styles.badge, {backgroundColor: color + '22'}]}>
      <Text style={[styles.badgeText, {color}]}>{label}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#0f0f0f',
  },
  header: {
    padding: 20,
    borderBottomWidth: 1,
    borderBottomColor: '#1f1f1f',
  },
  counter: {
    color: '#ffffff',
    fontSize: 24,
    fontWeight: '700',
  },
  overallBar: {
    height: 6,
    backgroundColor: '#262626',
    borderRadius: 3,
    marginTop: 12,
    overflow: 'hidden',
  },
  overallFill: {
    height: '100%',
    borderRadius: 3,
    backgroundColor: '#6366f1',
  },
  headerHint: {
    color: '#666666',
    fontSize: 13,
    marginTop: 8,
  },
  list: {
    padding: 16,
  },
  row: {
    backgroundColor: '#1a1a1a',
    borderRadius: 12,
    padding: 14,
    marginBottom: 12,
  },
  rowHeader: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    marginBottom: 10,
  },
  filename: {
    color: '#ffffff',
    fontSize: 15,
    fontWeight: '600',
    flex: 1,
    marginRight: 10,
  },
  badge: {
    paddingHorizontal: 8,
    paddingVertical: 3,
    borderRadius: 6,
  },
  badgeText: {
    fontSize: 11,
    fontWeight: '700',
  },
  bar: {
    height: 5,
    backgroundColor: '#262626',
    borderRadius: 3,
    overflow: 'hidden',
  },
  barFill: {
    height: '100%',
    borderRadius: 3,
  },
  metaRow: {
    flexDirection: 'row',
    alignItems: 'center',
    flexWrap: 'wrap',
    marginTop: 8,
  },
  meta: {
    color: '#ffffff',
    fontSize: 13,
    fontFamily: 'monospace',
    marginRight: 12,
  },
  metaDim: {
    color: '#777777',
    fontSize: 13,
    fontFamily: 'monospace',
    marginRight: 12,
  },
  metaWaiting: {
    color: '#777777',
    fontSize: 13,
    fontFamily: 'monospace',
    marginTop: 8,
  },
  error: {
    color: '#ef4444',
    fontSize: 12,
    marginTop: 8,
    paddingHorizontal: 16,
  },
  footer: {
    padding: 16,
    borderTopWidth: 1,
    borderTopColor: '#1f1f1f',
  },
  cta: {
    backgroundColor: '#6366f1',
    paddingVertical: 16,
    borderRadius: 12,
    alignItems: 'center',
  },
  ctaText: {
    color: '#ffffff',
    fontSize: 17,
    fontWeight: '700',
  },
});
