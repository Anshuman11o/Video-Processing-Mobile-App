import React, {useEffect, useState} from 'react';
import {View, Text, StyleSheet, ScrollView, ActivityIndicator} from 'react-native';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {getReel} from '../api/client';
import {useJobPolling} from '../hooks/useJobPolling';
import {
  ALL_STAGES,
  type ReelResponse,
  completedStageCount,
  jobErrorMessage,
} from '../types/api';

type PlayerScreenProps = NativeStackScreenProps<RootStackParamList, 'Player'>;

/**
 * Shows a job's progress and, once it finishes, the reel URL.
 *
 * It deliberately does NOT play the video. Playback is Stage 8B — see
 * [DECIDE 6] in docs/stage-plans/stage-7-upload-integration.md. Showing the URL
 * selectably is what proves the last link of the pipeline from the client that
 * exists to consume it: `GET /jobs/{id}/reel` returned 409 for every job in
 * this project's life until stage 6A, and this is the first time anything other
 * than curl has asked it for a real answer.
 */
export default function PlayerScreen({route}: PlayerScreenProps) {
  const {jobId} = route.params;
  const {job, error} = useJobPolling(jobId);
  const [reel, setReel] = useState<ReelResponse | null>(null);
  const [reelError, setReelError] = useState<string | null>(null);

  // Fetched separately from the job because the two endpoints disagree: the
  // reel reads DynamoDB directly while the job status is served from a 10s
  // Redis cache, so a job can be complete here before its cached status says so.
  useEffect(() => {
    if (job?.status !== 'completed' || reel) {
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const r = await getReel(jobId);
        if (!cancelled && r) {
          setReel(r);
        }
      } catch (err) {
        if (!cancelled) {
          setReelError(err instanceof Error ? err.message : String(err));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [job?.status, jobId, reel]);

  const failure = job ? jobErrorMessage(job) : undefined;

  return (
    <ScrollView style={styles.container} contentContainerStyle={styles.content}>
      <View style={styles.playerPlaceholder}>
        <Text style={styles.placeholderIcon}>▶</Text>
        <Text style={styles.placeholderText}>
          {reel ? 'Reel ready' : 'Processing'}
        </Text>
        <Text style={styles.placeholderSubtext}>
          Playback arrives in Stage 8B
        </Text>
      </View>

      <View style={styles.info}>
        <Text style={styles.label}>Job ID</Text>
        <Text style={styles.value} selectable>
          {jobId}
        </Text>

        <Text style={styles.label}>Status</Text>
        <Text style={styles.value}>
          {job ? job.status : 'loading…'}
          {job && !reel && job.status !== 'failed' ? (
            <ActivityIndicator size="small" color="#6366f1" />
          ) : null}
        </Text>

        {job ? (
          <>
            <Text style={styles.label}>Stages</Text>
            <Text style={styles.value}>
              {completedStageCount(job)}/{ALL_STAGES.length} complete
            </Text>
            {ALL_STAGES.map(stage => (
              <Text key={stage} style={styles.stageRow}>
                {stage}: {job.stages[stage]?.status ?? 'pending'}
              </Text>
            ))}
          </>
        ) : null}

        {reel ? (
          <>
            <Text style={styles.label}>HLS URL</Text>
            {/* Selectable so it can be copied into a real player — that is the
                verification this screen exists for until 8B lands. */}
            <Text style={styles.url} selectable>
              {reel.hls_url}
            </Text>
            {reel.thumbnail_url ? (
              <>
                <Text style={styles.label}>Thumbnail</Text>
                <Text style={styles.url} selectable>
                  {reel.thumbnail_url}
                </Text>
              </>
            ) : null}
          </>
        ) : null}

        {failure ? (
          <>
            <Text style={styles.label}>Failure</Text>
            <Text style={styles.error}>{failure}</Text>
          </>
        ) : null}

        {error ? <Text style={styles.error}>status: {error}</Text> : null}
        {reelError ? <Text style={styles.error}>reel: {reelError}</Text> : null}
      </View>
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#0f0f0f',
  },
  content: {
    paddingBottom: 40,
  },
  playerPlaceholder: {
    aspectRatio: 16 / 9,
    backgroundColor: '#1a1a1a',
    justifyContent: 'center',
    alignItems: 'center',
  },
  placeholderIcon: {
    fontSize: 48,
    color: '#444444',
    marginBottom: 12,
  },
  placeholderText: {
    color: '#666666',
    fontSize: 18,
    fontWeight: '600',
  },
  placeholderSubtext: {
    color: '#444444',
    fontSize: 13,
    marginTop: 4,
  },
  info: {
    padding: 20,
  },
  label: {
    color: '#666666',
    fontSize: 13,
    textTransform: 'uppercase',
    marginBottom: 4,
    marginTop: 16,
  },
  value: {
    color: '#ffffff',
    fontSize: 14,
    fontFamily: 'monospace',
  },
  stageRow: {
    color: '#888888',
    fontSize: 13,
    fontFamily: 'monospace',
    marginTop: 2,
  },
  url: {
    color: '#6366f1',
    fontSize: 13,
    fontFamily: 'monospace',
  },
  error: {
    color: '#ef4444',
    fontSize: 13,
    marginTop: 8,
  },
});
