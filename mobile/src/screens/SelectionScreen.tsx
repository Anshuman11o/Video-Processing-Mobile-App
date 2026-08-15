// The clips the user just picked, before anything has been uploaded.
//
// This screen is the "before" half of the app's claim: these are the original
// files, at their original resolution, with their own sound and no captions.
// Whatever the pipeline returns later is only meaningful next to this.
//
// Previews are tap-to-play, never autoplay. Five ExoPlayer instances starting
// at once will stall the emulator this is demonstrated on, and a wall of moving
// thumbnails is not more informative than one clip the user chose to watch.

import React, {useCallback, useMemo, useState} from 'react';
import {
  View,
  Text,
  FlatList,
  TouchableOpacity,
  StyleSheet,
} from 'react-native';
import Video, {type OnLoadData} from 'react-native-video';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {type PickedClip, totalBytes} from '../types/clips';
import {formatDuration, formatMB} from '../metrics/jobMetrics';

type SelectionScreenProps = NativeStackScreenProps<RootStackParamList, 'Selection'>;

/**
 * A clip's duration, once something has looked.
 *
 * `undefined` means not yet probed, `null` means the decoder could not say.
 * The distinction matters because the prober walks the list looking for the
 * first `undefined`, and a failed probe that stayed `undefined` would make it
 * retry the same broken file forever.
 */
type DurationMap = Record<string, number | null | undefined>;

export default function SelectionScreen({
  route,
  navigation,
}: SelectionScreenProps) {
  // Seeded from the route params but owned here: removing a clip has to change
  // what the upload button sends, and navigation params are not state.
  const [clips, setClips] = useState<PickedClip[]>(route.params.clips);
  const [durations, setDurations] = useState<DurationMap>({});
  const [previewId, setPreviewId] = useState<string | null>(null);

  const remove = useCallback((id: string) => {
    setPreviewId(current => (current === id ? null : current));
    setClips(current => current.filter(c => c.id !== id));
  }, []);

  /**
   * The next clip whose length is unknown.
   *
   * Durations are probed one at a time, and not at all while a preview is
   * open. The picker knows a file's size but not its length — only a decoder
   * does — so the only way to show "0:32" is to open the file. Doing that for
   * every clip at once is exactly the stampede the tap-to-play rule exists to
   * avoid, so it is a queue of one.
   */
  const probeClip = useMemo(() => {
    if (previewId !== null) {
      return null;
    }
    return clips.find(c => durations[c.id] === undefined) ?? null;
  }, [clips, durations, previewId]);

  const onProbed = useCallback((id: string, seconds: number | null) => {
    setDurations(current => ({...current, [id]: seconds}));
  }, []);

  const totalLabel = `${clips.length} clip${clips.length === 1 ? '' : 's'} · ${formatMB(
    totalBytes(clips),
  )}`;

  return (
    <View style={styles.container}>
      <FlatList
        data={clips}
        keyExtractor={clip => clip.id}
        ListHeaderComponent={
          <View style={styles.header}>
            <Text style={styles.headerTitle}>Ready to upload</Text>
            <Text style={styles.headerSubtitle}>{totalLabel}</Text>
            <Text style={styles.headerHint}>
              Tap a clip to preview the original — full resolution, with sound,
              no captions.
            </Text>
          </View>
        }
        renderItem={({item}) => (
          <ClipCard
            clip={item}
            durationSeconds={durations[item.id] ?? undefined}
            previewing={previewId === item.id}
            onTogglePreview={() =>
              setPreviewId(current => (current === item.id ? null : item.id))
            }
            onRemove={() => remove(item.id)}
          />
        )}
        ListEmptyComponent={
          <View style={styles.empty}>
            <Text style={styles.emptyText}>No clips left</Text>
            <Text style={styles.emptySubtext}>Go back and pick some videos.</Text>
          </View>
        }
        contentContainerStyle={styles.list}
      />

      {/* Off-screen, one at a time. Rendered outside the list so scrolling
          cannot unmount it half way through a probe. */}
      {probeClip ? (
        <DurationProbe
          key={probeClip.id}
          clip={probeClip}
          onResult={seconds => onProbed(probeClip.id, seconds)}
        />
      ) : null}

      <View style={styles.footer}>
        <TouchableOpacity
          style={[styles.cta, clips.length === 0 && styles.ctaDisabled]}
          disabled={clips.length === 0}
          onPress={() =>
            navigation.navigate('UploadProgress', {
              // The probed lengths ride along so the job list can show them
              // before the pipeline has produced any output of its own.
              clips: clips.map(c => ({
                ...c,
                durationSeconds: durations[c.id] ?? undefined,
              })),
            })
          }>
          <Text style={styles.ctaText}>
            Upload {clips.length} video{clips.length === 1 ? '' : 's'}
          </Text>
        </TouchableOpacity>
      </View>
    </View>
  );
}

function ClipCard({
  clip,
  durationSeconds,
  previewing,
  onTogglePreview,
  onRemove,
}: {
  clip: PickedClip;
  durationSeconds?: number;
  previewing: boolean;
  onTogglePreview: () => void;
  onRemove: () => void;
}) {
  return (
    <View style={styles.card}>
      <View style={styles.cardHeader}>
        <View style={styles.cardHeaderText}>
          <Text style={styles.filename} numberOfLines={1}>
            {clip.filename}
          </Text>
          <Text style={styles.meta}>
            {formatMB(clip.sizeBytes)} · {formatDuration(durationSeconds)}
          </Text>
        </View>
        <TouchableOpacity
          style={styles.removeButton}
          onPress={onRemove}
          hitSlop={{top: 10, bottom: 10, left: 10, right: 10}}>
          <Text style={styles.removeLabel}>✕</Text>
        </TouchableOpacity>
      </View>

      {previewing ? (
        <View style={styles.stage}>
          <Video
            // The local file, exactly as picked: no captions selected, not
            // muted, `contain` so nothing is cropped. This is the original.
            source={{uri: clip.fileUri}}
            style={styles.video}
            controls
            resizeMode="contain"
            paused={false}
            repeat={false}
          />
        </View>
      ) : (
        <TouchableOpacity style={styles.previewButton} onPress={onTogglePreview}>
          <Text style={styles.previewIcon}>▶</Text>
          <Text style={styles.previewLabel}>Tap to preview</Text>
        </TouchableOpacity>
      )}

      {previewing ? (
        <TouchableOpacity style={styles.closePreview} onPress={onTogglePreview}>
          <Text style={styles.closePreviewLabel}>Close preview</Text>
        </TouchableOpacity>
      ) : null}
    </View>
  );
}

/**
 * Open a file just far enough to learn how long it is.
 *
 * Paused and muted, one pixel of it on screen: ExoPlayer reports duration on
 * `onLoad` once it has prepared the media, whether or not it is playing, so
 * this never decodes a frame it does not have to. A one-pixel view rather than
 * `display: none` because a view with no layout is not attached, and an
 * unattached player never prepares.
 *
 * `onError` matters as much as `onLoad`: a file the decoder rejects has to
 * resolve to "unknown" so the queue moves on to the next clip.
 */
function DurationProbe({
  clip,
  onResult,
}: {
  clip: PickedClip;
  onResult: (seconds: number | null) => void;
}) {
  const onLoad = useCallback(
    (data: OnLoadData) => {
      const seconds = data.duration;
      onResult(
        typeof seconds === 'number' && Number.isFinite(seconds) && seconds > 0
          ? seconds
          : null,
      );
    },
    [onResult],
  );

  return (
    <Video
      source={{uri: clip.fileUri}}
      style={styles.probe}
      paused
      muted
      onLoad={onLoad}
      onError={() => onResult(null)}
    />
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    backgroundColor: '#0f0f0f',
  },
  list: {
    padding: 16,
    paddingBottom: 24,
  },
  header: {
    marginBottom: 16,
  },
  headerTitle: {
    color: '#ffffff',
    fontSize: 22,
    fontWeight: '700',
  },
  headerSubtitle: {
    color: '#6366f1',
    fontSize: 14,
    fontWeight: '600',
    marginTop: 4,
  },
  headerHint: {
    color: '#666666',
    fontSize: 13,
    marginTop: 8,
    lineHeight: 18,
  },
  card: {
    backgroundColor: '#1a1a1a',
    borderRadius: 12,
    padding: 12,
    marginBottom: 12,
  },
  cardHeader: {
    flexDirection: 'row',
    alignItems: 'center',
    marginBottom: 10,
  },
  cardHeaderText: {
    flex: 1,
    marginRight: 8,
  },
  filename: {
    color: '#ffffff',
    fontSize: 15,
    fontWeight: '600',
  },
  meta: {
    color: '#888888',
    fontSize: 13,
    marginTop: 2,
    fontFamily: 'monospace',
  },
  removeButton: {
    width: 30,
    height: 30,
    borderRadius: 15,
    borderWidth: 1,
    borderColor: '#333333',
    alignItems: 'center',
    justifyContent: 'center',
  },
  removeLabel: {
    color: '#888888',
    fontSize: 14,
  },
  previewButton: {
    aspectRatio: 16 / 9,
    borderRadius: 8,
    backgroundColor: '#0f0f0f',
    borderWidth: 1,
    borderColor: '#262626',
    alignItems: 'center',
    justifyContent: 'center',
  },
  previewIcon: {
    color: '#6366f1',
    fontSize: 30,
  },
  previewLabel: {
    color: '#666666',
    fontSize: 13,
    marginTop: 6,
  },
  stage: {
    aspectRatio: 16 / 9,
    borderRadius: 8,
    overflow: 'hidden',
    backgroundColor: '#000000',
  },
  video: {
    flex: 1,
  },
  closePreview: {
    alignSelf: 'flex-start',
    marginTop: 8,
    paddingHorizontal: 10,
    paddingVertical: 6,
  },
  closePreviewLabel: {
    color: '#6366f1',
    fontSize: 13,
    fontWeight: '600',
  },
  probe: {
    width: 1,
    height: 1,
    opacity: 0,
    position: 'absolute',
  },
  footer: {
    padding: 16,
    borderTopWidth: 1,
    borderTopColor: '#1f1f1f',
    backgroundColor: '#0f0f0f',
  },
  cta: {
    backgroundColor: '#6366f1',
    paddingVertical: 16,
    borderRadius: 12,
    alignItems: 'center',
  },
  ctaDisabled: {
    opacity: 0.4,
  },
  ctaText: {
    color: '#ffffff',
    fontSize: 17,
    fontWeight: '700',
  },
  empty: {
    alignItems: 'center',
    paddingVertical: 40,
  },
  emptyText: {
    color: '#ffffff',
    fontSize: 18,
    fontWeight: '600',
  },
  emptySubtext: {
    color: '#666666',
    fontSize: 14,
    marginTop: 6,
  },
});
