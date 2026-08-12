import React, {useCallback, useRef, useState} from 'react';
import {
  View,
  Text,
  TextInput,
  StyleSheet,
  ScrollView,
  ActivityIndicator,
  TouchableOpacity,
} from 'react-native';
import {useFocusEffect} from '@react-navigation/native';
import Video, {
  SelectedTrackType,
  type OnLoadData,
  type OnProgressData,
  type OnTextTrackDataChangedData,
  type OnTextTracksData,
  type OnVideoErrorData,
  type SelectedTrack,
  type SubtitleStyle,
  type TextTrack,
  type VideoRef,
} from 'react-native-video';
import type {NativeStackScreenProps} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';
import {useJobPolling} from '../hooks/useJobPolling';
import {useReel} from '../player/useReel';
import {
  type CueBoundary,
  cueOffsetMs,
  measureCueBoundary,
  videoRefProbe,
} from '../player/captionProbe';
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

/** White-on-white captions look exactly like no captions. */
const SUBTITLE_STYLE: SubtitleStyle = {
  fontSize: 18,
  paddingBottom: 16,
  opacity: 1,
  subtitlesFollowVideo: true,
};

/**
 * How far either side of the authored cue boundary the probe brackets.
 *
 * Wide enough to contain every offset this pipeline has produced — 6A's
 * original ~112 ms, and the candidates around the X-TIMESTAMP-MAP fix — while
 * staying inside the neighbouring cues so the bracket never lands in a gap.
 */
const PROBE_BRACKET_SECONDS = 0.15;

/** The mock transcript's first cue boundary; overridable for a real one. */
const DEFAULT_AUTHORED_BOUNDARY = '3.000';

interface ProbeResult {
  boundary: CueBoundary;
  authoredSeconds: number;
  offsetMs: number;
}

/**
 * Plays a finished reel, and shows enough of the player's own state to tell
 * apart the ways captions fail.
 *
 * The diagnostics block is not decoration. 6A recorded that a player showing no
 * captions is indistinguishable from a clip with none, and this project has now
 * shipped three bugs whose happy path looked green. Rendering the track list,
 * the cue text the renderer last delivered, and a measured cue boundary turns
 * "no captions" into a three-way diagnosis: no track in the playlist, a track
 * present but not selected, or a track selected that delivers no cues.
 */
export default function PlayerScreen({route}: PlayerScreenProps) {
  const {jobId} = route.params;
  const {job, error} = useJobPolling(jobId);
  const isCompleted = job?.status === 'completed';
  const {reel, notReady, error: reelError, refresh} = useReel(jobId, isCompleted);

  const videoRef = useRef<VideoRef | null>(null);
  const [paused, setPaused] = useState(false);
  const [textTracks, setTextTracks] = useState<TextTrack[] | null>(null);
  const [currentCue, setCurrentCue] = useState<string | null>(null);
  const [position, setPosition] = useState(0);
  const [duration, setDuration] = useState<number | null>(null);
  const [playbackError, setPlaybackError] = useState<string | null>(null);

  // Read synchronously by the probe between seeks, so it cannot go through
  // component state — a re-render is not guaranteed to have landed by then.
  const cueRef = useRef<string | null>(null);

  const [authoredInput, setAuthoredInput] = useState(DEFAULT_AUTHORED_BOUNDARY);
  const [probing, setProbing] = useState(false);
  const [probeResult, setProbeResult] = useState<ProbeResult | null>(null);
  const [probeError, setProbeError] = useState<string | null>(null);

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

  const onTextTrackDataChanged = useCallback(
    (e: OnTextTrackDataChangedData) => {
      // Named `subtitleTracks` on the wire, but it carries the active cue's
      // text — see ReactExoplayerView.onCues. It only fires for NON-empty cue
      // groups, so this value never clears on its own.
      cueRef.current = e.subtitleTracks;
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

  const runProbe = useCallback(async () => {
    const authoredSeconds = Number.parseFloat(authoredInput);
    if (!Number.isFinite(authoredSeconds)) {
      setProbeError(`"${authoredInput}" is not a time in seconds`);
      return;
    }
    setProbing(true);
    setProbeError(null);
    setProbeResult(null);
    // The measurement depends on it: a seek only settles at a definite
    // position while the player is stopped.
    setPaused(true);
    try {
      const probe = videoRefProbe(
        seconds => videoRef.current?.seek(seconds),
        () => cueRef.current,
      );
      const boundary = await measureCueBoundary(probe, {
        lo: authoredSeconds - PROBE_BRACKET_SECONDS,
        hi: authoredSeconds + PROBE_BRACKET_SECONDS,
      });
      setProbeResult({
        boundary,
        authoredSeconds,
        offsetMs: cueOffsetMs(boundary, authoredSeconds),
      });
    } catch (err) {
      setProbeError(err instanceof Error ? err.message : String(err));
    } finally {
      setProbing(false);
    }
  }, [authoredInput]);

  const failure = job ? jobErrorMessage(job) : undefined;

  return (
    <ScrollView style={styles.container} contentContainerStyle={styles.content}>
      <View style={styles.stage}>
        {reel && !playbackError ? (
          <Video
            // Re-mounting on a new URL is what makes `refresh` a real retry.
            key={reel.hls_url}
            ref={videoRef}
            source={{uri: reel.hls_url}}
            style={styles.video}
            paused={paused}
            controls
            resizeMode="contain"
            selectedTextTrack={SELECTED_TEXT_TRACK}
            subtitleStyle={SUBTITLE_STYLE}
            progressUpdateInterval={250}
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
            {/* Kept selectable: it is still the fastest way to check the same
                stream in another player when the two disagree. */}
            <Text style={styles.url} selectable>
              {reel.hls_url}
            </Text>
            {reel.thumbnail_url ? (
              <>
                {/* Shown as text, deliberately NOT loaded as a poster. It
                    points into dayreel-processed, which has no public-read
                    policy — it resolves here only because LocalStack serves
                    unsigned GETs to any bucket, and would 403 on real S3.
                    See [DECIDE 2]. */}
                <Text style={styles.label}>Thumbnail (not fetched)</Text>
                <Text style={styles.url} selectable>
                  {reel.thumbnail_url}
                </Text>
              </>
            ) : null}
          </>
        ) : null}

        {reel ? (
          <CaptionDiagnostics
            textTracks={textTracks}
            currentCue={currentCue}
            position={position}
            duration={duration}
            authoredInput={authoredInput}
            onAuthoredInput={setAuthoredInput}
            probing={probing}
            probeResult={probeResult}
            probeError={probeError}
            onProbe={runProbe}
            onResume={() => setPaused(false)}
            paused={paused}
          />
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
 * The three-way caption diagnosis, plus the measured cue boundary.
 *
 * The offset number is the reason this stage exists: 6A could not measure its
 * own caption defect because ffmpeg ignores X-TIMESTAMP-MAP, so a real player
 * is the only oracle. See src/player/captionProbe.ts for why it bisects rather
 * than watching the screen.
 */
function CaptionDiagnostics({
  textTracks,
  currentCue,
  position,
  duration,
  authoredInput,
  onAuthoredInput,
  probing,
  probeResult,
  probeError,
  onProbe,
  onResume,
  paused,
}: {
  textTracks: TextTrack[] | null;
  currentCue: string | null;
  position: number;
  duration: number | null;
  authoredInput: string;
  onAuthoredInput: (v: string) => void;
  probing: boolean;
  probeResult: ProbeResult | null;
  probeError: string | null;
  onProbe: () => void;
  onResume: () => void;
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
            #{t.index} {t.language ?? '??'} {t.title ?? '(untitled)'}{' '}
            {t.selected ? '✓ selected' : '—'}
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

      <Text style={styles.label}>Caption offset probe</Text>
      <View style={styles.probeRow}>
        <TextInput
          style={styles.probeInput}
          value={authoredInput}
          onChangeText={onAuthoredInput}
          keyboardType="numeric"
          placeholder="authored boundary (s)"
          placeholderTextColor="#555555"
        />
        <TouchableOpacity
          style={styles.probeButton}
          onPress={onProbe}
          disabled={probing}>
          <Text style={styles.retryLabel}>{probing ? 'measuring…' : 'Measure'}</Text>
        </TouchableOpacity>
        {paused ? (
          <TouchableOpacity style={styles.probeButton} onPress={onResume}>
            <Text style={styles.retryLabel}>Resume</Text>
          </TouchableOpacity>
        ) : null}
      </View>

      {probeResult ? (
        <>
          <Text style={styles.value} selectable>
            offset {probeResult.offsetMs >= 0 ? '+' : ''}
            {probeResult.offsetMs.toFixed(1)} ms (
            {probeResult.offsetMs >= 0 ? 'late' : 'early'})
          </Text>
          <Text style={styles.stageRow} selectable>
            boundary {probeResult.boundary.seconds.toFixed(4)}s ±
            {(probeResult.boundary.toleranceSeconds * 1000).toFixed(1)}ms,
            authored {probeResult.authoredSeconds.toFixed(3)}s,{' '}
            {probeResult.boundary.probeCount} probes
          </Text>
          <Text style={styles.stageRow} selectable>
            {probeResult.boundary.before ?? 'none'} →{' '}
            {probeResult.boundary.after ?? 'none'}
          </Text>
        </>
      ) : null}
      {probeError ? <Text style={styles.error}>{probeError}</Text> : null}
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
  url: {
    color: '#6366f1',
    fontSize: 13,
    fontFamily: 'monospace',
  },
  probeRow: {
    flexDirection: 'row',
    alignItems: 'center',
    marginTop: 4,
  },
  probeInput: {
    flex: 1,
    color: '#ffffff',
    fontFamily: 'monospace',
    fontSize: 14,
    backgroundColor: '#1a1a1a',
    borderRadius: 8,
    paddingHorizontal: 10,
    paddingVertical: 6,
    marginRight: 8,
  },
  probeButton: {
    paddingHorizontal: 14,
    paddingVertical: 8,
    borderRadius: 8,
    backgroundColor: '#6366f1',
    marginRight: 8,
  },
  error: {
    color: '#ef4444',
    fontSize: 13,
    marginTop: 8,
  },
});
