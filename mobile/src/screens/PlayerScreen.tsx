import React from 'react';
import {View, Text, StyleSheet} from 'react-native';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';

type PlayerScreenProps = NativeStackScreenProps<RootStackParamList, 'Player'>;

export default function PlayerScreen({route}: PlayerScreenProps) {
  const {jobId} = route.params;

  return (
    <View style={styles.container}>
      <View style={styles.playerPlaceholder}>
        <Text style={styles.placeholderIcon}>▶</Text>
        <Text style={styles.placeholderText}>HLS Player</Text>
        <Text style={styles.placeholderSubtext}>
          Will be implemented in Stage 6
        </Text>
      </View>

      <View style={styles.info}>
        <Text style={styles.label}>Job ID</Text>
        <Text style={styles.value}>{jobId}</Text>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#0f0f0f',
  },
  playerPlaceholder: {
    aspectRatio: 9 / 16,
    backgroundColor: '#1a1a1a',
    justifyContent: 'center',
    alignItems: 'center',
    maxHeight: '60%',
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
  },
  value: {
    color: '#ffffff',
    fontSize: 14,
    fontFamily: 'monospace',
  },
});
