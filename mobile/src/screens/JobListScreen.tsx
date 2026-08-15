import React, {useCallback, useRef, useState} from 'react';
import {
  View,
  Text,
  FlatList,
  TouchableOpacity,
  StyleSheet,
  ActivityIndicator,
  RefreshControl,
} from 'react-native';
import {useFocusEffect} from '@react-navigation/native';
import type {NativeStackNavigationProp} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {
  ALL_STAGES,
  type Job,
  type JobStatus,
  completedStageCount,
  isTerminal,
  jobErrorMessage,
} from '../types/api';
import {getJobStatus} from '../api/client';
import {blobJobIndexStore} from '../storage/blobJobIndexStore';
import {type JobIndexEntry, loadIndex} from '../storage/jobIndex';
import {StageTracker} from '../components/StageTracker';
import {useTicker} from '../hooks/useTicker';
import {formatDuration, formatMB} from '../metrics/jobMetrics';

type JobListScreenProps = {
  navigation: NativeStackNavigationProp<RootStackParamList, 'JobList'>;
};

const STATUS_COLORS: Record<JobStatus, string> = {
  pending: '#6b7280',
  uploading: '#f59e0b',
  processing: '#3b82f6',
  completed: '#22c55e',
  failed: '#ef4444',
};

/** A job we know the id of but have not fetched (or could not fetch) yet. */
interface JobRow {
  entry: JobIndexEntry;
  job: Job | null;
  error?: string;
}

/** How often to re-poll the list while any job on it is still running. */
const LIST_POLL_MS = 2000;

function JobCard({
  row,
  nowMs,
  onPress,
}: {
  row: JobRow;
  nowMs: number;
  onPress: () => void;
}) {
  const {entry, job} = row;

  // The local index carries the filename, size and length, so a row is
  // readable before — or without — the API answering for it. That matters most
  // during an upload, when `GET /jobs/{id}` has almost nothing to say and this
  // is exactly when someone is watching.
  const sizeLabel = entry.size_bytes ?? job?.size_bytes;
  const meta = [
    sizeLabel !== undefined ? formatMB(sizeLabel) : null,
    entry.duration_seconds !== undefined
      ? formatDuration(entry.duration_seconds)
      : null,
  ]
    .filter(Boolean)
    .join(' · ');

  if (!job) {
    return (
      <TouchableOpacity style={styles.card} onPress={onPress}>
        <Text style={styles.filename} numberOfLines={1}>
          {entry.filename}
        </Text>
        {meta ? <Text style={styles.meta}>{meta}</Text> : null}
        <Text style={styles.stageInfo}>
          {row.error ? `unavailable — ${row.error}` : 'loading…'}
        </Text>
      </TouchableOpacity>
    );
  }

  const statusColor = STATUS_COLORS[job.status] ?? '#6b7280';
  const completedStages = completedStageCount(job);
  // Fixed at four rather than counting the map's keys: a job whose stages have
  // not all been written yet would otherwise show "1/1 stages complete".
  const totalStages = ALL_STAGES.length;
  const failure = jobErrorMessage(job);
  const done = job.status === 'completed';

  return (
    <TouchableOpacity style={styles.card} onPress={onPress}>
      <View style={styles.cardHeader}>
        <View style={styles.cardHeaderText}>
          <Text style={styles.filename} numberOfLines={1}>
            {job.filename}
          </Text>
          {meta ? <Text style={styles.meta}>{meta}</Text> : null}
        </View>
        {done ? (
          // Deliberately louder than the other statuses. "Is it finished yet"
          // is the only question this list is asked, and a lowercase chip the
          // same size as `processing` made it something you had to read rather
          // than see.
          <View style={styles.doneBadge}>
            <Text style={styles.doneBadgeText}>✓ COMPLETED</Text>
          </View>
        ) : (
          <View style={[styles.statusBadge, {backgroundColor: statusColor + '22'}]}>
            <Text style={[styles.statusText, {color: statusColor}]}>
              {job.status}
            </Text>
          </View>
        )}
      </View>

      <StageTracker job={job} nowMs={nowMs} compact />

      <Text style={styles.stageInfo}>
        {completedStages}/{totalStages} stages complete
      </Text>

      {failure ? <Text style={styles.errorText}>{failure}</Text> : null}
    </TouchableOpacity>
  );
}

export default function JobListScreen({navigation}: JobListScreenProps) {
  const [rows, setRows] = useState<JobRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);

  // The compact trackers carry no seconds counters, so this only has to be
  // fast enough that a stage changing colour does not wait on the next poll.
  const now = useTicker(1000);

  /**
   * Whether anything on the list is still moving.
   *
   * Held in a ref, not read from state, because the poll loop that has to stop
   * is created once per focus — reading `rows` from the closure would give it
   * the empty list it was created with, and it would poll forever.
   */
  const anyRunning = useRef(true);

  const load = useCallback(async () => {
    const entries = await loadIndex(blobJobIndexStore);

    // Fetched in parallel: there is no GET /jobs, so this is N requests by
    // construction, and the list is capped at 100 entries.
    const fetched = await Promise.all(
      entries.map(async (entry): Promise<JobRow> => {
        try {
          return {entry, job: await getJobStatus(entry.job_id)};
        } catch (err) {
          // One unreachable job must not blank the whole list — it may simply
          // have been wiped from LocalStack while the index survived.
          return {
            entry,
            job: null,
            error: err instanceof Error ? err.message : String(err),
          };
        }
      }),
    );

    anyRunning.current = fetched.some(r => !r.job || !isTerminal(r.job.status));
    setRows(fetched);
    setLoading(false);
  }, []);

  // Refresh on focus, and keep polling only while something is unfinished.
  // Left running, this is N requests every 2s for as long as the screen is
  // open — the client-side version of the hot loop `config/free-tier.md`
  // forbids.
  useFocusEffect(
    useCallback(() => {
      let cancelled = false;
      anyRunning.current = true;

      const timer = setInterval(() => {
        if (cancelled || !anyRunning.current) {
          clearInterval(timer);
          return;
        }
        load();
      }, LIST_POLL_MS);

      load();

      return () => {
        cancelled = true;
        clearInterval(timer);
      };
    }, [load]),
  );

  if (loading) {
    return (
      <View style={styles.centered}>
        <ActivityIndicator size="large" color="#6366f1" />
      </View>
    );
  }

  if (rows.length === 0) {
    return (
      <View style={styles.centered}>
        <Text style={styles.emptyText}>No jobs yet</Text>
        <Text style={styles.emptySubtext}>Select videos to get started</Text>
      </View>
    );
  }

  return (
    <View style={styles.container}>
      <FlatList
        data={rows}
        keyExtractor={item => item.entry.job_id}
        refreshControl={
          <RefreshControl
            refreshing={refreshing}
            tintColor="#6366f1"
            onRefresh={async () => {
              setRefreshing(true);
              await load();
              setRefreshing(false);
            }}
          />
        }
        renderItem={({item}) => (
          <JobCard
            row={item}
            nowMs={now}
            onPress={() =>
              // The detail screen, not the player. The player is only worth
              // opening once there is something to play, and it says nothing
              // about a job that is still running — which is most of the time
              // a user spends here.
              navigation.navigate('JobDetail', {
                jobId: item.entry.job_id,
                filename: item.entry.filename,
              })
            }
          />
        )}
        contentContainerStyle={styles.list}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#0f0f0f',
  },
  centered: {
    flex: 1,
    justifyContent: 'center',
    alignItems: 'center',
    backgroundColor: '#0f0f0f',
  },
  list: {
    padding: 16,
  },
  card: {
    backgroundColor: '#1a1a1a',
    borderRadius: 12,
    padding: 16,
    marginBottom: 12,
  },
  cardHeader: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 14,
  },
  cardHeaderText: {
    flex: 1,
    marginRight: 8,
  },
  filename: {
    color: '#ffffff',
    fontSize: 16,
    fontWeight: '600',
  },
  meta: {
    color: '#777777',
    fontSize: 12,
    fontFamily: 'monospace',
    marginTop: 2,
  },
  statusBadge: {
    paddingHorizontal: 10,
    paddingVertical: 4,
    borderRadius: 8,
  },
  statusText: {
    fontSize: 12,
    fontWeight: '600',
    textTransform: 'uppercase',
  },
  doneBadge: {
    backgroundColor: '#22c55e',
    paddingHorizontal: 10,
    paddingVertical: 5,
    borderRadius: 8,
  },
  doneBadgeText: {
    color: '#0f0f0f',
    fontSize: 12,
    fontWeight: '800',
  },
  stageInfo: {
    color: '#666666',
    fontSize: 13,
    marginTop: 10,
  },
  errorText: {
    color: '#ef4444',
    fontSize: 12,
    marginTop: 6,
  },
  emptyText: {
    color: '#ffffff',
    fontSize: 20,
    fontWeight: '600',
    marginBottom: 8,
  },
  emptySubtext: {
    color: '#666666',
    fontSize: 14,
  },
});
