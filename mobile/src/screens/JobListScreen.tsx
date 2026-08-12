import React, {useEffect, useState} from 'react';
import {
  View,
  Text,
  FlatList,
  TouchableOpacity,
  StyleSheet,
  ActivityIndicator,
} from 'react-native';
import type {NativeStackNavigationProp} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import type {Job, JobStatus} from '../types/api';
import {getMockJobs} from '../api/client';

type JobListScreenProps = {
  navigation: NativeStackNavigationProp<RootStackParamList, 'JobList'>;
};

const STATUS_COLORS: Record<JobStatus, string> = {
  uploading: '#f59e0b',
  processing: '#3b82f6',
  completed: '#22c55e',
  failed: '#ef4444',
};

function getCompletedStages(job: Job): number {
  return Object.values(job.stages).filter(s => s.status === 'complete').length;
}

function JobCard({job, onPress}: {job: Job; onPress: () => void}) {
  const statusColor = STATUS_COLORS[job.status];
  const completedStages = getCompletedStages(job);
  const totalStages = Object.keys(job.stages).length;

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
    </TouchableOpacity>
  );
}

export default function JobListScreen({navigation}: JobListScreenProps) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    // TODO: Replace with real API call
    const mockJobs = getMockJobs();
    setJobs(mockJobs);
    setLoading(false);
  }, []);

  if (loading) {
    return (
      <View style={styles.centered}>
        <ActivityIndicator size="large" color="#6366f1" />
      </View>
    );
  }

  if (jobs.length === 0) {
    return (
      <View style={styles.centered}>
        <Text style={styles.emptyText}>No jobs yet</Text>
        <Text style={styles.emptySubtext}>
          Select a video to get started
        </Text>
      </View>
    );
  }

  return (
    <View style={styles.container}>
      <FlatList
        data={jobs}
        keyExtractor={item => item.job_id}
        renderItem={({item}) => (
          <JobCard
            job={item}
            onPress={() => {
              if (item.status === 'completed') {
                navigation.navigate('Player', {jobId: item.job_id});
              }
            }}
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
