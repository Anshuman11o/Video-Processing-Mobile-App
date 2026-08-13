import React, {useCallback, useState} from 'react';
import {
  View,
  Text,
  StyleSheet,
  ScrollView,
  ActivityIndicator,
  TouchableOpacity,
} from 'react-native';
import {useFocusEffect} from '@react-navigation/native';
import Video, {
  SelectedTrackType,
  SelectedVideoTrackType,
  type OnLoadData,
  type OnProgressData,
  type OnTextTrackDataChangedData,
  type OnTextTracksData,
  type OnVideoErrorData,
  type OnVideoTracksData,
  type SelectedTrack,
  type SelectedVideoTrack,
  type SubtitleStyle,
  type TextTrack,
  type VideoTrack,
} from 'react-native-video';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {useJobPolling} from '../hooks/useJobPolling';
import {useReel} from '../player/useReel';
import {ALL_STAGES, completedStageCount, jobErrorMessage} from '../types/api';

type PlayerScreenProps = NativeStackScreenProps<RootStackParamList, 'Player'>;

/**
 * Force the English caption track on, rather than letting ExoPlayer decide.
 *
 * [DECIDE 4]. ExoPlayer's DefaultTrackSelector derives its text preference from
 * TrackSelectionParameters, which default to the device's system captioning
 * setting — off on a fresh emulator. The playlist's DEFAULT=YES,AUTOSELECT=YES
 * states the content's intent but does not override that, so leaving this to
 * the default risks a video that plays with no captions, no error and no
 * warning: visually identical to a clip that has none, and to a malformed
 * subtitle rendition.
 *
 * Selected by language rather than by index so it survives the playlist gaining
 * a second rendition. Held at module scope because a fresh object each render
 * re-applies the prop to the native view on every commit.
 *
 * Typed as `SelectedTrack`, not `SelectedTextTrack`: the library exports both,
 * they disagree, and only `SelectedTrack` is what the prop actually accepts —
 * `SelectedTextTrack` permits a `'title'` selector that `SelectedTrack` does
 * not. Using the enum rather than a bare string keeps that mismatch a compile
 * error instead of a silently ignored prop.
 */
const SELECTED_TEXT_TRACK: SelectedTrack = {
  type: SelectedTrackType.LANGUAGE,
  value: 'en',
};

/**
 * Let ExoPlayer's adaptive bitrate logic pick the rendition.
 *
 * The default, and it should stay the default: ABR is what the three renditions
 * in the master playlist are FOR. A manual pick pins the player to one variant
 * for the rest of playback, so it belongs to the user choosing it, not to the
 * screen opening.
 *
 * `SelectedVideoTrack` is the type the prop takes — checked against
 * lib/types/video.d.ts rather than assumed, because the text-track equivalent
 * exports two similar names that disagree (see SELECTED_TEXT_TRACK above).
 * Video exports only this one, but the enum is used rather than the bare string
 * 'auto' for the same reason: a wrong value has to fail at compile time, since
 * at runtime an unrecognised type is simply ignored.
 */
const AUTO_VIDEO_TRACK: SelectedVideoTrack = {
  type: SelectedVideoTrackType.AUTO,
};

/** White-on-white captions look exactly like no captions. */
const SUBTITLE_STYLE: SubtitleStyle = {
  fontSize: 18,
  paddingBottom: 16,
  opacity: 1,
  subtitlesFollowVideo: true,
};

/**
 * Plays a finished reel, and shows enough of the player's own state to tell
 * apart the ways captions fail.
 *
 * The diagnostics block is not decoration. 6A recorded that a player showing no
 * captions is indistinguishable from a clip with none, and this project has now
 * shipped three bugs whose happy path looked green. Rendering the track list and
 * the cue text the renderer last delivered turns "no captions" into a three-way
 * diagnosis: no track in the playlist, a track present but not selected, or a
 * track selected that delivers no cues.
 */
export default function PlayerScreen({route}: PlayerScreenProps) {
  const {jobId} = route.params;
  const {job, error} = useJobPolling(jobId);
  const isCompleted = job?.status === 'completed';
  const {reel, notReady, error: reelError, refresh} = useReel(jobId, isCompleted);

  const [paused, setPaused] = useState(false);
  const [textTracks, setTextTracks] = useState<TextTrack[] | null>(null);
  const [videoTracks, setVideoTracks] = useState<VideoTrack[] | null>(null);
  const [selectedVideoTrack, setSelectedVideoTrack] =
    useState<SelectedVideoTrack>(AUTO_VIDEO_TRACK);
  const [currentCue, setCurrentCue] = useState<string | null>(null);
  const [position, setPosition] = useState(0);
  const [duration, setDuration] = useState<number | null>(null);
  const [playbackError, setPlaybackError] = useState<string | null>(null);

  // Going back unmounts this screen and releases the player, but pushing
  // another screen on top does not. Without this, audio keeps playing over the
  // job list.
  useFocusEffect(
    useCallback(() => {
      return () => setPaused(true);
    }, []),
  );

  const onTextTracks = useCallback((e: OnTextTracksData) => {
    setTextTracks([...e.textTracks]);
  }, []);

  const onVideoTracks = useCallback((e: OnVideoTracksData) => {
    // Highest first, so the list reads the way the master playlist is written
    // rather than in whatever order the track selector enumerated groups.
    setVideoTracks(
      [...e.videoTracks].sort((a, b) => (b.height ?? 0) - (a.height ?? 0)),
    );
  }, []);

  const onTextTrackDataChanged = useCallback(
    (e: OnTextTrackDataChangedData) => {
      // Named `subtitleTracks` on the wire, but it carries the active cue's
      // text — see ReactExoplayerView.onCues. It only fires for NON-empty cue
      // groups, so this value never clears on its own.
      setCurrentCue(e.subtitleTracks);
    },
    [],
  );

  const onLoad = useCallback((e: OnLoadData) => {
    setDuration(e.duration);
    setPlaybackError(null);
  }, []);

  const onProgress = useCallback((e: OnProgressData) => {
    setPosition(e.currentTime);
  }, []);

  const onError = useCallback((e: OnVideoErrorData) => {
    setPlaybackError(JSON.stringify(e.error));
  }, []);

  const failure = job ? jobErrorMessage(job) : undefined;

  return (
    <ScrollView style={styles.container} contentContainerStyle={styles.content}>
      <View style={styles.stage}>
        {reel && !playbackError ? (
          <Video
            // Re-mounting on a new URL is what makes `refresh` a real retry.
            key={reel.hls_url}
            source={{uri: reel.hls_url}}
            style={styles.video}
            paused={paused}
            controls
            resizeMode="contain"
            selectedTextTrack={SELECTED_TEXT_TRACK}
            selectedVideoTrack={selectedVideoTrack}
            subtitleStyle={SUBTITLE_STYLE}
            progressUpdateInterval={250}
            onVideoTracks={onVideoTracks}
            onTextTracks={onTextTracks}
            onTextTrackDataChanged={onTextTrackDataChanged}
            onLoad={onLoad}
            onProgress={onProgress}
            onError={onError}
          />
        ) : (
          <PlaceholderStage
            playbackError={playbackError}
            failure={failure}
            notReady={notReady}
            status={job?.status}
            onRetry={() => {
              setPlaybackError(null);
              refresh();
            }}
          />
        )}
      </View>

      <View style={styles.info}>
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
            <VideoTrackPicker
              videoTracks={videoTracks}
              selected={selectedVideoTrack}
              onSelect={setSelectedVideoTrack}
            />
            <CaptionDiagnostics
              textTracks={textTracks}
              currentCue={currentCue}
              position={position}
              duration={duration}
              paused={paused}
            />
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

/** What occupies the video area when there is nothing to play. */
function PlaceholderStage({
  playbackError,
  failure,
  notReady,
  status,
  onRetry,
}: {
  playbackError: string | null;
  failure: string | undefined;
  notReady: boolean;
  status: string | undefined;
  onRetry: () => void;
}) {
  if (playbackError) {
    return (
      <View style={styles.placeholder}>
        <Text style={styles.placeholderIcon}>⚠</Text>
        <Text style={styles.placeholderText}>Playback failed</Text>
        <Text style={styles.placeholderSubtext} selectable>
          {playbackError}
        </Text>
        <TouchableOpacity style={styles.retryButton} onPress={onRetry}>
          <Text style={styles.retryLabel}>Retry</Text>
        </TouchableOpacity>
      </View>
    );
  }

  if (failure || status === 'failed') {
    return (
      <View style={styles.placeholder}>
        <Text style={styles.placeholderIcon}>✕</Text>
        <Text style={styles.placeholderText}>Job failed</Text>
        <Text style={styles.placeholderSubtext}>{failure ?? 'no reel'}</Text>
      </View>
    );
  }

  return (
    <View style={styles.placeholder}>
      <ActivityIndicator size="large" color="#6366f1" />
      <Text style={styles.placeholderText}>
        {notReady ? 'Preparing reel' : 'Processing'}
      </Text>
      <Text style={styles.placeholderSubtext}>
        {notReady
          ? 'the job is done; the reel endpoint has not caught up'
          : `status: ${status ?? 'loading…'}`}
      </Text>
    </View>
  );
}

/**
 * The renditions the master playlist offers, and which one is playing.
 *
 * Without this there is no way to tell a three-rendition ladder from a
 * one-rendition one: ExoPlayer switches variants silently and the default
 * control bar exposes audio and subtitle tracks but not video.
 *
 * The active row comes from `selected` — this screen's own state — and NOT from
 * each track's `selected` flag. That flag is always false on Android:
 * ReactExoplayerView.exoplayerVideoTrackToGenericVideoTrack builds every
 * VideoTrack without ever calling setSelected, so trusting it would render a
 * list where nothing is ever marked.
 */
function VideoTrackPicker({
  videoTracks,
  selected,
  onSelect,
}: {
  videoTracks: VideoTrack[] | null;
  selected: SelectedVideoTrack;
  onSelect: (track: SelectedVideoTrack) => void;
}) {
  const isAuto = selected.type === SelectedVideoTrackType.AUTO;

  return (
    <>
      <Text style={styles.label}>Video quality</Text>
      <TouchableOpacity
        style={[styles.trackRow, isAuto && styles.trackRowActive]}
        onPress={() => onSelect(AUTO_VIDEO_TRACK)}>
        <Text style={isAuto ? styles.trackTextActive : styles.trackText}>
          Auto{isAuto ? ' ✓' : ''}
        </Text>
        <Text style={styles.trackDetail}>adaptive</Text>
      </TouchableOpacity>

      {videoTracks === null ? (
        <Text style={styles.stageRow}>waiting for onVideoTracks…</Text>
      ) : videoTracks.length === 0 ? (
        <Text style={styles.error}>none reported by the player</Text>
      ) : (
        videoTracks.map(t => {
          // Matched on height because that is what the RESOLUTION selector
          // sends: setSelectedTrack compares `format.height == value`.
          const active = !isAuto && selected.value === t.height;
          return (
            <TouchableOpacity
              key={t.index}
              style={[styles.trackRow, active && styles.trackRowActive]}
              onPress={() =>
                onSelect({
                  // Selected by resolution rather than by index, for the same
                  // reason the text track is selected by language: the index is
                  // whatever order the track selector happened to enumerate.
                  type: SelectedVideoTrackType.RESOLUTION,
                  value: t.height,
                })
              }>
              <Text style={active ? styles.trackTextActive : styles.trackText}>
                {t.height}p{active ? ' ✓' : ''}
              </Text>
              <Text style={styles.trackDetail}>
                {t.width}×{t.height}
                {t.bitrate ? ` · ${(t.bitrate / 1_000_000).toFixed(1)} Mbps` : ''}
              </Text>
            </TouchableOpacity>
          );
        })
      )}
    </>
  );
}

/**
 * The three-way caption diagnosis: no track in the playlist, a track present
 * but not selected, or a track selected that delivers no cues.
 */
function CaptionDiagnostics({
  textTracks,
  currentCue,
  position,
  duration,
  paused,
}: {
  textTracks: TextTrack[] | null;
  currentCue: string | null;
  position: number;
  duration: number | null;
  paused: boolean;
}) {
  return (
    <>
      <Text style={styles.label}>Text tracks</Text>
      {textTracks === null ? (
        <Text style={styles.stageRow}>waiting for onTextTracks…</Text>
      ) : textTracks.length === 0 ? (
        // Distinct from "no captions rendered": this says the playlist offered
        // the player nothing to select.
        <Text style={styles.error}>none reported by the player</Text>
      ) : (
        textTracks.map(t => (
          <Text key={t.index} style={styles.stageRow}>
            #{t.index} {t.language ?? '??'} {t.title ?? '(untitled)'}
            {/* Only the positive case is shown. With `controls` set,
                ReactExoplayerView reports text tracks through
                getBasicTextTrackInfo, which calls setSelected(false) on every
                one of them — "let PlayerView handle it". Printing a "—" for
                false would therefore assert "not selected" about a track that
                is in fact selected and rendering cues. */}
            {t.selected ? ' ✓ selected' : ''}
          </Text>
        ))
      )}

      <Text style={styles.label}>Current cue</Text>
      <Text style={currentCue ? styles.value : styles.stageRow}>
        {currentCue ?? 'none delivered yet'}
      </Text>

      <Text style={styles.label}>Position</Text>
      <Text style={styles.value}>
        {position.toFixed(3)}s{duration !== null ? ` / ${duration.toFixed(3)}s` : ''}
        {paused ? ' (paused)' : ''}
      </Text>
    </>
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
  stage: {
    aspectRatio: 16 / 9,
    backgroundColor: '#000000',
  },
  video: {
    flex: 1,
  },
  placeholder: {
    flex: 1,
    backgroundColor: '#1a1a1a',
    justifyContent: 'center',
    alignItems: 'center',
    paddingHorizontal: 24,
  },
  placeholderIcon: {
    fontSize: 44,
    color: '#444444',
    marginBottom: 12,
  },
  placeholderText: {
    color: '#888888',
    fontSize: 18,
    fontWeight: '600',
    marginTop: 8,
  },
  placeholderSubtext: {
    color: '#555555',
    fontSize: 12,
    marginTop: 6,
    textAlign: 'center',
  },
  retryButton: {
    marginTop: 14,
    paddingHorizontal: 18,
    paddingVertical: 8,
    borderRadius: 8,
    backgroundColor: '#6366f1',
  },
  retryLabel: {
    color: '#ffffff',
    fontSize: 13,
    fontWeight: '600',
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
  trackRow: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    paddingHorizontal: 12,
    paddingVertical: 10,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: '#262626',
    backgroundColor: '#1a1a1a',
    marginTop: 6,
  },
  trackRowActive: {
    borderColor: '#6366f1',
    backgroundColor: '#6366f122',
  },
  trackText: {
    color: '#cccccc',
    fontSize: 14,
    fontFamily: 'monospace',
  },
  trackTextActive: {
    color: '#ffffff',
    fontSize: 14,
    fontWeight: '600',
    fontFamily: 'monospace',
  },
  trackDetail: {
    color: '#777777',
    fontSize: 12,
    fontFamily: 'monospace',
  },
  error: {
    color: '#ef4444',
    fontSize: 13,
    marginTop: 8,
  },
});
