import React, {useCallback, useEffect, useState} from 'react';
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

function JobCard({row, onPress}: {row: JobRow; onPress: () => void}) {
  const {entry, job} = row;

  if (!job) {
    return (
      <TouchableOpacity style={styles.card} onPress={onPress}>
        <Text style={styles.filename} numberOfLines={1}>
          {entry.filename}
        </Text>
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

  return (
    <TouchableOpacity style={styles.card} onPress={onPress}>
      <View style={styles.cardHeader}>
        <Text style={styles.filename} numberOfLines={1}>
          {job.filename}
        </Text>
        <View style={[styles.statusBadge, {backgroundColor: statusColor + '22'}]}>
          <Text style={[styles.statusText, {color: statusColor}]}>
            {job.status}
          </Text>
        </View>
      </View>

      <View style={styles.progressBar}>
        <View
          style={[
            styles.progressFill,
            {
              width: `${(completedStages / totalStages) * 100}%`,
              backgroundColor: statusColor,
            },
          ]}
        />
      </View>

      <Text style={styles.stageInfo}>
        {completedStages}/{totalStages} stages complete
      </Text>

      {failure ? <Text style={styles.errorText}>{failure}</Text> : null}
    </TouchableOpacity>
  );
}

/** How often to re-poll the list while any job on it is still running. */
const LIST_POLL_MS = 2000;

export default function JobListScreen({navigation}: JobListScreenProps) {
  const [rows, setRows] = useState<JobRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);

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

    setRows(fetched);
    setLoading(false);
  }, []);

  // Refresh on focus, and keep polling only while something is unfinished.
  useFocusEffect(
    useCallback(() => {
      let cancelled = false;
      let timer: ReturnType<typeof setInterval> | null = null;

      const tick = async () => {
        await load();
        if (cancelled) {
          return;
        }
      };

      tick();
      timer = setInterval(() => {
        if (cancelled) {
          return;
        }
        tick();
      }, LIST_POLL_MS);

      return () => {
        cancelled = true;
        if (timer) {
          clearInterval(timer);
        }
      };
    }, [load]),
  );

  // Stop polling once every known job has settled. Left running, this is a
  // request every 2s for as long as the screen is open.
  const anyRunning = rows.some(r => !r.job || !isTerminal(r.job.status));
  useEffect(() => {
    if (!anyRunning && rows.length > 0) {
      setLoading(false);
    }
  }, [anyRunning, rows.length]);

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
        <Text style={styles.emptySubtext}>Select a video to get started</Text>
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
            onPress={() =>
              navigation.navigate('Player', {jobId: item.entry.job_id})
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
    marginBottom: 12,
  },
  filename: {
    color: '#ffffff',
    fontSize: 16,
    fontWeight: '600',
    flex: 1,
    marginRight: 8,
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
  progressBar: {
    height: 4,
    backgroundColor: '#333333',
    borderRadius: 2,
    marginBottom: 8,
  },
  progressFill: {
    height: '100%',
    borderRadius: 2,
  },
  stageInfo: {
    color: '#666666',
    fontSize: 13,
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
