// One job, while it runs.
//
// This screen exists so the pipeline is visible. Tapping a job used to open the
// player, which meant the only thing the app ever showed about four stages of
// processing was whether the result played — the work was invisible and the
// wait was unexplained.
//
// Three metrics, and deliberately only three:
//   M3  elapsed time    — the big clock, ticking locally
//   M1  stage tracker   — four pills, the running one counting
//   M6  upload progress — megabytes, parts, throughput
//
// When the job finishes there is a Watch button and nothing navigates on its
// own. Auto-advancing to the player would replace the finished metrics with a
// video the moment they became worth reading.

import React, {useEffect, useRef} from 'react';
import {
  View,
  Text,
  StyleSheet,
  ScrollView,
  ActivityIndicator,
  TouchableOpacity,
} from 'react-native';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {useJobPolling} from '../hooks/useJobPolling';
import {useUploadStatus} from '../hooks/useUploadStatus';
import {useTicker} from '../hooks/useTicker';
import {StageTracker} from '../components/StageTracker';
import {
  type Job,
  completedStageCount,
  isTerminal,
  jobErrorMessage,
  ALL_STAGES,
} from '../types/api';
import {
  formatClock,
  formatMB,
  jobElapsedMs,
  uploadProgressMetrics,
  uploadSummary,
} from '../metrics/jobMetrics';

type JobDetailScreenProps = NativeStackScreenProps<RootStackParamList, 'JobDetail'>;

export default function JobDetailScreen({
  route,
  navigation,
}: JobDetailScreenProps) {
  const {jobId, filename} = route.params;
  const {job, error, refresh} = useJobPolling(jobId);

  const terminal = job !== null && isTerminal(job.status);

  // The clock the screen actually runs on. Every seconds figure here is
  // computed from this and the timestamps of the last poll, never from when a
  // poll arrived — `GET /jobs/{id}` is served from a server-side cache, so
  // poll-derived counters advance in cache-sized jumps.
  const now = useTicker(1000, !terminal);

  /**
   * When this device first saw the job finish.
   *
   * Only used when a job reaches a terminal status with no stage timestamp to
   * freeze on — a validate failure, typically. Without it the "total" would
   * keep counting up after the job was over.
   */
  const observedEnd = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (terminal && observedEnd.current === undefined) {
      observedEnd.current = Date.now();
    }
  }, [terminal]);

  if (!job) {
    return (
      <View style={styles.centered}>
        <ActivityIndicator size="large" color="#6366f1" />
        <Text style={styles.loadingName}>{filename ?? jobId}</Text>
        {error ? <Text style={styles.error}>{error}</Text> : null}
      </View>
    );
  }

  const elapsed = jobElapsedMs(job, now, observedEnd.current);
  const failure = jobErrorMessage(job);
  const completed = job.status === 'completed';

  return (
    <ScrollView style={styles.container} contentContainerStyle={styles.content}>
      <Text style={styles.filename} numberOfLines={2}>
        {job.filename}
      </Text>
      <Text style={styles.subline}>
        {formatMB(job.size_bytes)}
        {job.output?.duration_seconds
          ? ` · ${Math.round(job.output.duration_seconds)}s of video`
          : ''}
      </Text>

      {/* M3 */}
      <View style={styles.clockBlock}>
        <Text style={styles.clock}>{formatClock(elapsed)}</Text>
        <Text style={styles.clockLabel}>
          {terminal ? 'total elapsed' : 'elapsed'}
        </Text>
        <StatusChip job={job} />
      </View>

      {/* M1 */}
      <Text style={styles.sectionLabel}>Pipeline</Text>
      <StageTracker job={job} nowMs={now} />
      <Text style={styles.sectionNote}>
        {completedStageCount(job)}/{ALL_STAGES.length} stages complete
      </Text>

      {/* M6 */}
      <Text style={styles.sectionLabel}>Upload</Text>
      <UploadBlock job={job} />

      {failure ? (
        <>
          <Text style={styles.sectionLabel}>Failure</Text>
          <Text style={styles.error}>{failure}</Text>
        </>
      ) : null}

      {error ? <Text style={styles.error}>status: {error}</Text> : null}

      {completed ? (
        <TouchableOpacity
          style={styles.watchButton}
          onPress={() => navigation.navigate('Player', {jobId})}>
          <Text style={styles.watchLabel}>▶  Watch</Text>
        </TouchableOpacity>
      ) : (
        <TouchableOpacity style={styles.refreshButton} onPress={refresh}>
          <Text style={styles.refreshLabel}>Refresh now</Text>
        </TouchableOpacity>
      )}
    </ScrollView>
  );
}

function StatusChip({job}: {job: Job}) {
  const color =
    job.status === 'completed'
      ? '#22c55e'
      : job.status === 'failed'
        ? '#ef4444'
        : '#6366f1';
  return (
    <View style={[styles.statusChip, {backgroundColor: color + '22'}]}>
      {!isTerminal(job.status) ? (
        <ActivityIndicator size="small" color={color} style={styles.chipSpinner} />
      ) : null}
      <Text style={[styles.statusChipText, {color}]}>{job.status}</Text>
    </View>
  );
}

/**
 * M6, in whichever of its two forms applies.
 *
 * Live while the bytes are moving, and a summary once they are not — the
 * metrics have to stay on screen for the whole job, so this block never
 * disappears once the upload is over. The live source is the device's own
 * uploader; the summary comes from the job. They are never mixed, because a
 * half-live half-recorded row would be a number nobody could interpret.
 */
function UploadBlock({job}: {job: Job}) {
  const uploading = job.status === 'pending' || job.status === 'uploading';
  const live = useUploadStatus(job.job_id, uploading);

  if (live) {
    const metrics = uploadProgressMetrics({
      fraction: live.fraction,
      totalBytes: job.size_bytes,
      uploadedParts: live.uploadedParts,
      totalParts: live.totalParts || job.upload?.total_parts,
    });
    return (
      <View style={styles.uploadBlock}>
        <View style={styles.bar}>
          <View style={[styles.barFill, {width: `${live.fraction * 100}%`}]} />
        </View>
        <Text style={styles.uploadPrimary}>{metrics.bytesLabel}</Text>
        {metrics.partsLabel ? (
          <Text style={styles.uploadSecondary}>
            parts {metrics.partsLabel} · {live.state.toLowerCase()}
          </Text>
        ) : null}
      </View>
    );
  }

  const summary = uploadSummary(job);
  // Nothing known and still uploading means the transfer belongs to another
  // installation of the app; empty is the honest bar for that, not full.
  const barWidth = uploading ? '0%' : '100%';
  return (
    <View style={styles.uploadBlock}>
      <View style={styles.bar}>
        <View
          style={[styles.barFill, styles.barFillDone, {width: barWidth}]}
        />
      </View>
      <Text style={styles.uploadPrimary}>
        {uploading
          ? `waiting · ${summary.sizeLabel}`
          : `${summary.sizeLabel} uploaded`}
      </Text>
      <Text style={styles.uploadSecondary}>
        {[
          summary.partsLabel ? `parts ${summary.partsLabel}` : null,
          summary.durationLabel,
          summary.throughputLabel,
        ]
          .filter(Boolean)
          .join(' · ') || 'no upload metrics recorded'}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#0f0f0f',
  },
  content: {
    padding: 20,
    paddingBottom: 48,
  },
  centered: {
    flex: 1,
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: '#0f0f0f',
  },
  loadingName: {
    color: '#666666',
    fontSize: 14,
    marginTop: 16,
  },
  filename: {
    color: '#ffffff',
    fontSize: 20,
    fontWeight: '700',
  },
  subline: {
    color: '#666666',
    fontSize: 13,
    marginTop: 4,
    fontFamily: 'monospace',
  },
  clockBlock: {
    alignItems: 'center',
    paddingVertical: 24,
  },
  clock: {
    color: '#ffffff',
    fontSize: 56,
    fontWeight: '200',
    fontFamily: 'monospace',
    letterSpacing: 2,
  },
  clockLabel: {
    color: '#666666',
    fontSize: 12,
    textTransform: 'uppercase',
    letterSpacing: 1,
    marginTop: 2,
  },
  statusChip: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingHorizontal: 12,
    paddingVertical: 5,
    borderRadius: 999,
    marginTop: 14,
  },
  chipSpinner: {
    marginRight: 6,
  },
  statusChipText: {
    fontSize: 12,
    fontWeight: '700',
    textTransform: 'uppercase',
  },
  sectionLabel: {
    color: '#666666',
    fontSize: 12,
    textTransform: 'uppercase',
    letterSpacing: 1,
    marginTop: 24,
    marginBottom: 10,
  },
  sectionNote: {
    color: '#777777',
    fontSize: 12,
    fontFamily: 'monospace',
    marginTop: 10,
  },
  uploadBlock: {
    backgroundColor: '#1a1a1a',
    borderRadius: 12,
    padding: 14,
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
    backgroundColor: '#6366f1',
  },
  barFillDone: {
    backgroundColor: '#22c55e',
  },
  uploadPrimary: {
    color: '#ffffff',
    fontSize: 15,
    fontFamily: 'monospace',
    marginTop: 10,
  },
  uploadSecondary: {
    color: '#777777',
    fontSize: 12,
    fontFamily: 'monospace',
    marginTop: 4,
  },
  watchButton: {
    backgroundColor: '#6366f1',
    paddingVertical: 16,
    borderRadius: 12,
    alignItems: 'center',
    marginTop: 32,
  },
  watchLabel: {
    color: '#ffffff',
    fontSize: 17,
    fontWeight: '700',
  },
  refreshButton: {
    borderWidth: 1,
    borderColor: '#262626',
    paddingVertical: 14,
    borderRadius: 12,
    alignItems: 'center',
    marginTop: 32,
  },
  refreshLabel: {
    color: '#777777',
    fontSize: 14,
  },
  error: {
    color: '#ef4444',
    fontSize: 13,
    marginTop: 8,
  },
});
