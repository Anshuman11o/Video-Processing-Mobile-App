// M1: validate → extract → transcribe → package.
//
// Completed stages are filled, the running one is highlighted and carries a
// live seconds counter, and everything else is an outline. The counter is
// ticked from `started_at` by whatever passes `nowMs` in — `metrics` only
// records a stage's duration once it *finishes*, so there is no
// partially-elapsed number on the wire to display.
//
// Shared between the job list, where it is a compact strip, and the job detail
// screen, where it carries the numbers.

import React from 'react';
import {View, Text, StyleSheet} from 'react-native';

import type {Job} from '../types/api';
import {type StagePill, formatSeconds, stagePills} from '../metrics/jobMetrics';

const COLORS = {
  completed: '#22c55e',
  current: '#6366f1',
  failed: '#ef4444',
  pending: '#3f3f46',
} as const;

export function StageTracker({
  job,
  nowMs,
  compact = false,
}: {
  job: Job;
  nowMs: number;
  compact?: boolean;
}) {
  const pills = stagePills(job, nowMs);

  return (
    <View style={styles.row}>
      {pills.map((pill, i) => (
        <React.Fragment key={pill.name}>
          {i > 0 ? (
            <View
              style={[
                styles.connector,
                // The link into a stage is lit once that stage has started, so
                // the strip reads as a filling track rather than four
                // unrelated chips.
                (pill.state === 'completed' || pill.state === 'current') &&
                  styles.connectorOn,
              ]}
            />
          ) : null}
          <Pill pill={pill} compact={compact} />
        </React.Fragment>
      ))}
    </View>
  );
}

function Pill({pill, compact}: {pill: StagePill; compact: boolean}) {
  const color = COLORS[pill.state];
  const filled = pill.state === 'completed' || pill.state === 'failed';
  // A filled pill's label sits on the fill, so it takes the background colour;
  // a pending one is dimmer than its own outline so the row reads as unstarted.
  const textColor = filled
    ? '#0f0f0f'
    : pill.state === 'pending'
      ? '#71717a'
      : color;

  return (
    <View
      style={[
        styles.pill,
        compact && styles.pillCompact,
        {borderColor: color},
        filled && {backgroundColor: color},
        pill.state === 'current' && {backgroundColor: color + '33'},
      ]}>
      <Text
        style={[
          styles.pillText,
          compact && styles.pillTextCompact,
          {color: textColor},
        ]}
        numberOfLines={1}>
        {compact ? pill.name.slice(0, 3) : pill.name}
      </Text>

      {!compact && pill.state === 'current' && pill.elapsedMs !== undefined ? (
        <Text style={[styles.pillTimer, {color}]}>
          {formatSeconds(pill.elapsedMs)}
        </Text>
      ) : null}

      {!compact && pill.state === 'completed' && pill.durationMs !== undefined ? (
        <Text style={styles.pillDuration}>{formatSeconds(pill.durationMs)}</Text>
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  row: {
    flexDirection: 'row',
    alignItems: 'center',
  },
  connector: {
    flex: 1,
    height: 2,
    backgroundColor: '#27272a',
    marginHorizontal: 2,
  },
  connectorOn: {
    backgroundColor: '#22c55e',
  },
  pill: {
    borderWidth: 1.5,
    borderRadius: 999,
    paddingHorizontal: 10,
    paddingVertical: 6,
    alignItems: 'center',
    minWidth: 62,
  },
  pillCompact: {
    paddingHorizontal: 7,
    paddingVertical: 3,
    minWidth: 0,
    borderWidth: 1,
  },
  pillText: {
    fontSize: 11,
    fontWeight: '700',
    textTransform: 'uppercase',
  },
  pillTextCompact: {
    fontSize: 9,
  },
  pillTimer: {
    fontSize: 11,
    fontFamily: 'monospace',
    fontWeight: '700',
    marginTop: 2,
  },
  pillDuration: {
    fontSize: 10,
    fontFamily: 'monospace',
    color: '#0f0f0f',
    marginTop: 2,
  },
});
