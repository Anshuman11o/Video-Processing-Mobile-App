# Stage 2B: Mobile Shell (React Native)

> **Substrate superseded in one place.** The app work below is current. The
> sample `hls_url` in the mock data points at the old emulator endpoint; reels
> now come from the HLS bucket on real S3 (`HLS_BASE_URL`), with no CDN in
> front. See `infra/CONTEXT.md`.

> **Run in parallel with:** Stage 2A (Go API)
> **Depends on:** Stage 1B (Infrastructure running for eventual testing)
> **Estimated time:** 30 minutes
> **Blocks:** Stage 7 (Upload Integration)

## Aim

Scaffold the React Native app with video picker, job list UI, and API client stub.
The app should run in Android emulator, pick videos from gallery, and display a
job list (with mock data initially). Upload integration comes in Stage 7.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `mobile/` | Create | Full RN project scaffold |
| `mobile/src/screens/` | Create | `HomeScreen.tsx`, `JobListScreen.tsx`, `PlayerScreen.tsx` |
| `mobile/src/components/` | Create | `JobCard.tsx`, `VideoPlayer.tsx` |
| `mobile/src/services/` | Create | `api.ts`, `storage.ts` |
| `mobile/src/types/` | Create | `job.ts` |
| `mobile/` | Create | `CONTEXT.md` |

---

## App Structure

```
mobile/
├── android/                    # Android native project
├── ios/                        # iOS native project (stub)
├── src/
│   ├── App.tsx                 # Root component with navigation
│   ├── screens/
│   │   ├── HomeScreen.tsx      # Video picker + record button
│   │   ├── JobListScreen.tsx   # List of jobs with status
│   │   └── PlayerScreen.tsx    # HLS video player
│   ├── components/
│   │   ├── JobCard.tsx         # Single job item
│   │   ├── VideoPlayer.tsx     # HLS player wrapper
│   │   └── ProgressBar.tsx     # Upload/processing progress
│   ├── services/
│   │   ├── api.ts              # Backend API client
│   │   ├── storage.ts          # AsyncStorage wrapper
│   │   └── upload.ts           # Upload queue (stub for now)
│   ├── types/
│   │   └── job.ts              # TypeScript types matching backend
│   ├── hooks/
│   │   └── useJobs.ts          # Job list state management
│   └── config/
│       └── index.ts            # API URL, feature flags
├── package.json
├── tsconfig.json
├── babel.config.js
├── metro.config.js
└── CONTEXT.md
```

---

## Screen Designs

### HomeScreen

```
┌─────────────────────────────────────┐
│  DayReel                    [Jobs]  │  <- Header with nav to JobList
├─────────────────────────────────────┤
│                                     │
│                                     │
│         ┌───────────────┐           │
│         │               │           │
│         │   Camera      │           │  <- Future: camera preview
│         │   Preview     │           │
│         │               │           │
│         └───────────────┘           │
│                                     │
│                                     │
├─────────────────────────────────────┤
│                                     │
│    [  Pick from Gallery  ]          │  <- Opens image picker
│                                     │
│    [     Record Video    ]          │  <- Future: camera capture
│                                     │
└─────────────────────────────────────┘
```

### JobListScreen

```
┌─────────────────────────────────────┐
│  ←  Jobs                            │
├─────────────────────────────────────┤
│ ┌─────────────────────────────────┐ │
│ │ 📹 beach-sunset.mp4             │ │
│ │ ████████████░░░░ 75%            │ │  <- Upload progress
│ │ Status: Uploading...            │ │
│ └─────────────────────────────────┘ │
│ ┌─────────────────────────────────┐ │
│ │ 📹 birthday-party.mp4           │ │
│ │ ✓ Complete                      │ │
│ │ [▶ Play Reel]                   │ │  <- Opens PlayerScreen
│ └─────────────────────────────────┘ │
│ ┌─────────────────────────────────┐ │
│ │ 📹 cooking-tutorial.mp4         │ │
│ │ ⏳ Processing: extract          │ │
│ │ Stage 2 of 4                    │ │
│ └─────────────────────────────────┘ │
│                                     │
└─────────────────────────────────────┘
```

### PlayerScreen

```
┌─────────────────────────────────────┐
│  ←  beach-sunset.mp4                │
├─────────────────────────────────────┤
│                                     │
│  ┌─────────────────────────────────┐│
│  │                                 ││
│  │                                 ││
│  │      HLS Video Player           ││  <- react-native-video
│  │      with captions              ││
│  │                                 ││
│  │                                 ││
│  └─────────────────────────────────┘│
│                                     │
│   advancement bar                   │
│  ▶ 00:15 / 01:30                   │
│                                     │
│  Quality: Auto (720p)              │  <- Adaptive bitrate
│                                     │
└─────────────────────────────────────┘
```

---

## TypeScript Types

### `mobile/src/types/job.ts`

```typescript
export type JobStatus = 'pending' | 'uploading' | 'processing' | 'complete' | 'failed';
export type StageStatus = 'pending' | 'processing' | 'complete' | 'failed';
export type StageName = 'validate' | 'extract' | 'transcribe' | 'package';

export interface StageState {
  status: StageStatus;
  started_at?: string;
  completed_at?: string;
  attempts: number;
  error?: string;
}

export interface Job {
  job_id: string;
  filename: string;
  size_bytes: number;
  status: JobStatus;
  created_at: string;
  stages: Record<StageName, StageState>;
  output?: {
    hls_url?: string;
    thumbnail_url?: string;
    duration_seconds?: number;
  };
  // Local-only fields
  local_uri?: string;
  upload_progress?: number;
}

export interface CreateJobRequest {
  filename: string;
  size_bytes: number;
  content_type: string;
}

export interface CreateJobResponse {
  job_id: string;
  upload_id: string;
  upload_urls: Array<{
    part_number: number;
    url: string;
  }>;
  part_size: number;
  expires_in: number;
}

export interface CompleteUploadRequest {
  upload_id: string;
  parts: Array<{
    part_number: number;
    etag: string;
  }>;
}
```

---

## API Client

### `mobile/src/services/api.ts`

```typescript
import { Job, CreateJobRequest, CreateJobResponse, CompleteUploadRequest } from '../types/job';
import { API_URL } from '../config';

class ApiClient {
  private baseUrl: string;

  constructor(baseUrl: string) {
    this.baseUrl = baseUrl;
  }

  async createJob(request: CreateJobRequest): Promise<CreateJobResponse> {
    const response = await fetch(`${this.baseUrl}/jobs`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(request),
    });
    if (!response.ok) throw new Error(`Create job failed: ${response.status}`);
    return response.json();
  }

  async completeUpload(jobId: string, request: CompleteUploadRequest): Promise<void> {
    const response = await fetch(`${this.baseUrl}/jobs/${jobId}/complete`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(request),
    });
    if (!response.ok) throw new Error(`Complete upload failed: ${response.status}`);
  }

  async getJob(jobId: string): Promise<Job> {
    const response = await fetch(`${this.baseUrl}/jobs/${jobId}`);
    if (!response.ok) throw new Error(`Get job failed: ${response.status}`);
    return response.json();
  }

  async getReel(jobId: string): Promise<{ hls_url: string; thumbnail_url?: string }> {
    const response = await fetch(`${this.baseUrl}/jobs/${jobId}/reel`);
    if (!response.ok) throw new Error(`Get reel failed: ${response.status}`);
    return response.json();
  }
}

export const api = new ApiClient(API_URL);
```

---

## Key Dependencies

```json
{
  "dependencies": {
    "react": "18.2.0",
    "react-native": "0.73.x",
    "@react-navigation/native": "^6.x",
    "@react-navigation/native-stack": "^6.x",
    "react-native-screens": "^3.x",
    "react-native-safe-area-context": "^4.x",
    "react-native-image-picker": "^7.x",
    "react-native-video": "^6.x",
    "@react-native-async-storage/async-storage": "^1.x"
  },
  "devDependencies": {
    "@types/react": "^18.x",
    "typescript": "^5.x"
  }
}
```

---

## Navigation Structure

```typescript
// App.tsx
import { NavigationContainer } from '@react-navigation/native';
import { createNativeStackNavigator } from '@react-navigation/native-stack';

export type RootStackParamList = {
  Home: undefined;
  JobList: undefined;
  Player: { jobId: string };
};

const Stack = createNativeStackNavigator<RootStackParamList>();

export default function App() {
  return (
    <NavigationContainer>
      <Stack.Navigator initialRouteName="Home">
        <Stack.Screen name="Home" component={HomeScreen} options={{ title: 'DayReel' }} />
        <Stack.Screen name="JobList" component={JobListScreen} options={{ title: 'Jobs' }} />
        <Stack.Screen name="Player" component={PlayerScreen} />
      </Stack.Navigator>
    </NavigationContainer>
  );
}
```

---

## HomeScreen Implementation

```typescript
// screens/HomeScreen.tsx
import React from 'react';
import { View, Button, StyleSheet, Alert } from 'react-native';
import { launchImageLibrary } from 'react-native-image-picker';
import { useNavigation } from '@react-navigation/native';

export function HomeScreen() {
  const navigation = useNavigation();

  const pickVideo = async () => {
    const result = await launchImageLibrary({
      mediaType: 'video',
      quality: 1,
    });

    if (result.assets && result.assets[0]) {
      const video = result.assets[0];
      Alert.alert(
        'Video Selected',
        `${video.fileName}\n${(video.fileSize! / 1024 / 1024).toFixed(1)} MB`,
        [
          { text: 'Cancel', style: 'cancel' },
          {
            text: 'Upload',
            onPress: () => {
              // TODO: Stage 7 - actual upload
              console.log('Would upload:', video.uri);
              navigation.navigate('JobList');
            }
          },
        ]
      );
    }
  };

  return (
    <View style={styles.container}>
      <View style={styles.buttonContainer}>
        <Button title="Pick from Gallery" onPress={pickVideo} />
        <View style={styles.spacer} />
        <Button title="View Jobs" onPress={() => navigation.navigate('JobList')} />
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, justifyContent: 'center', padding: 20 },
  buttonContainer: { gap: 16 },
  spacer: { height: 16 },
});
```

---

## JobListScreen with Mock Data

```typescript
// screens/JobListScreen.tsx
import React from 'react';
import { View, FlatList, StyleSheet } from 'react-native';
import { JobCard } from '../components/JobCard';
import { Job } from '../types/job';

// Mock data for initial development
const MOCK_JOBS: Job[] = [
  {
    job_id: '1',
    filename: 'beach-sunset.mp4',
    size_bytes: 15728640,
    status: 'uploading',
    created_at: new Date().toISOString(),
    stages: {
      validate: { status: 'pending', attempts: 0 },
      extract: { status: 'pending', attempts: 0 },
      transcribe: { status: 'pending', attempts: 0 },
      package: { status: 'pending', attempts: 0 },
    },
    upload_progress: 0.75,
  },
  {
    job_id: '2',
    filename: 'birthday-party.mp4',
    size_bytes: 52428800,
    status: 'complete',
    created_at: new Date(Date.now() - 3600000).toISOString(),
    stages: {
      validate: { status: 'complete', attempts: 1, completed_at: new Date().toISOString() },
      extract: { status: 'complete', attempts: 1, completed_at: new Date().toISOString() },
      transcribe: { status: 'complete', attempts: 1, completed_at: new Date().toISOString() },
      package: { status: 'complete', attempts: 1, completed_at: new Date().toISOString() },
    },
    output: {
      hls_url: 'http://localhost:4566/dayreel-hls-output/2/master.m3u8',
    },
  },
  {
    job_id: '3',
    filename: 'cooking-tutorial.mp4',
    size_bytes: 104857600,
    status: 'processing',
    created_at: new Date(Date.now() - 1800000).toISOString(),
    stages: {
      validate: { status: 'complete', attempts: 1 },
      extract: { status: 'processing', started_at: new Date().toISOString(), attempts: 1 },
      transcribe: { status: 'pending', attempts: 0 },
      package: { status: 'pending', attempts: 0 },
    },
  },
];

export function JobListScreen() {
  // TODO: Replace with real API polling in Stage 7
  const jobs = MOCK_JOBS;

  return (
    <View style={styles.container}>
      <FlatList
        data={jobs}
        keyExtractor={(item) => item.job_id}
        renderItem={({ item }) => <JobCard job={item} />}
        contentContainerStyle={styles.list}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: '#f5f5f5' },
  list: { padding: 16 },
});
```

---

## JobCard Component

```typescript
// components/JobCard.tsx
import React from 'react';
import { View, Text, TouchableOpacity, StyleSheet } from 'react-native';
import { useNavigation } from '@react-navigation/native';
import { Job, StageName } from '../types/job';

const STAGE_ORDER: StageName[] = ['validate', 'extract', 'transcribe', 'package'];

function getStageProgress(job: Job): { current: number; total: number; label: string } {
  const total = STAGE_ORDER.length;
  let current = 0;
  let label = '';

  for (const stage of STAGE_ORDER) {
    if (job.stages[stage].status === 'complete') {
      current++;
    } else if (job.stages[stage].status === 'processing') {
      label = stage;
      break;
    }
  }

  return { current, total, label };
}

export function JobCard({ job }: { job: Job }) {
  const navigation = useNavigation();
  const progress = getStageProgress(job);

  const handlePress = () => {
    if (job.status === 'complete' && job.output?.hls_url) {
      navigation.navigate('Player', { jobId: job.job_id });
    }
  };

  return (
    <TouchableOpacity style={styles.card} onPress={handlePress} disabled={job.status !== 'complete'}>
      <Text style={styles.filename}>📹 {job.filename}</Text>

      {job.status === 'uploading' && job.upload_progress !== undefined && (
        <>
          <View style={styles.progressBar}>
            <View style={[styles.progressFill, { width: `${job.upload_progress * 100}%` }]} />
          </View>
          <Text style={styles.status}>Uploading: {Math.round(job.upload_progress * 100)}%</Text>
        </>
      )}

      {job.status === 'processing' && (
        <Text style={styles.status}>
          ⏳ Processing: {progress.label} ({progress.current + 1} of {progress.total})
        </Text>
      )}

      {job.status === 'complete' && (
        <Text style={styles.complete}>✓ Complete - Tap to play</Text>
      )}

      {job.status === 'failed' && (
        <Text style={styles.failed}>✗ Failed</Text>
      )}
    </TouchableOpacity>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: 'white',
    borderRadius: 8,
    padding: 16,
    marginBottom: 12,
    shadowColor: '#000',
    shadowOffset: { width: 0, height: 1 },
    shadowOpacity: 0.1,
    shadowRadius: 2,
    elevation: 2,
  },
  filename: { fontSize: 16, fontWeight: '600', marginBottom: 8 },
  progressBar: {
    height: 8,
    backgroundColor: '#e0e0e0',
    borderRadius: 4,
    marginBottom: 8,
    overflow: 'hidden',
  },
  progressFill: { height: '100%', backgroundColor: '#4CAF50' },
  status: { color: '#666', fontSize: 14 },
  complete: { color: '#4CAF50', fontSize: 14, fontWeight: '500' },
  failed: { color: '#f44336', fontSize: 14, fontWeight: '500' },
});
```

---

## Tasks

1. [ ] Initialize React Native project: `npx react-native init DayReel --template react-native-template-typescript`
2. [ ] Install dependencies: navigation, image-picker, video, async-storage
3. [ ] Create `mobile/src/types/job.ts`
4. [ ] Create `mobile/src/config/index.ts`
5. [ ] Create `mobile/src/services/api.ts`
6. [ ] Create `mobile/src/components/JobCard.tsx`
7. [ ] Create `mobile/src/components/ProgressBar.tsx`
8. [ ] Create `mobile/src/screens/HomeScreen.tsx`
9. [ ] Create `mobile/src/screens/JobListScreen.tsx`
10. [ ] Create `mobile/src/screens/PlayerScreen.tsx` (stub)
11. [ ] Update `mobile/src/App.tsx` with navigation
12. [ ] Configure Android permissions for camera/storage
13. [ ] Run in Android emulator: `npx react-native run-android`
14. [ ] Verify video picker works
15. [ ] Create `mobile/CONTEXT.md`

---

## Test

```bash
# Start Metro bundler
cd mobile && npx react-native start

# In another terminal, run on Android
cd mobile && npx react-native run-android

# Manual verification:
# 1. App opens to HomeScreen
# 2. Tap "Pick from Gallery" - picker opens
# 3. Select a video - alert shows filename and size
# 4. Tap "View Jobs" - navigates to JobListScreen
# 5. Mock jobs display with different states
# 6. Tap complete job - would navigate to Player (stub)
```

---

## Verification Checklist

- [ ] App builds and runs in Android emulator
- [ ] HomeScreen displays with two buttons
- [ ] Video picker opens and returns video metadata
- [ ] Navigation works between Home ↔ JobList
- [ ] JobListScreen displays mock jobs
- [ ] JobCard shows correct status for each state (uploading, processing, complete, failed)
- [ ] Progress bar displays for uploading jobs
- [ ] Stage progress displays for processing jobs
- [ ] Complete jobs show "tap to play" hint
- [ ] TypeScript types match backend API contract

---

## Claude Code Implementation Plan

### Recommended Approach: Subagent for RN Init, Then Direct Implementation

React Native project initialization is slow and verbose. Use a subagent to handle
the init and dependency installation, then switch to direct implementation for
the TypeScript code.

### Execution Steps

```
Phase 1: Project scaffold (use subagent)
- Initialize RN project
- Install all dependencies
- Configure Android permissions
- Verify build works

Phase 2: TypeScript code (direct, parallel writes)
2a. Write src/types/job.ts
2b. Write src/config/index.ts
2c. Write src/services/api.ts

Phase 3: Components (parallel writes)
3a. Write src/components/JobCard.tsx
3b. Write src/components/ProgressBar.tsx

Phase 4: Screens (parallel writes)
4a. Write src/screens/HomeScreen.tsx
4b. Write src/screens/JobListScreen.tsx
4c. Write src/screens/PlayerScreen.tsx

Phase 5: Wire up
5. Update src/App.tsx with navigation
6. Run and test in emulator
7. Write CONTEXT.md
```

### Why Subagent for Phase 1?

- `npx react-native init` takes 2-3 minutes and produces verbose output
- Dependency installation is slow
- Android build is slow first time
- Agent can handle this autonomously while we plan Phase 2

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 2 | types/job.ts, config/index.ts, services/api.ts |
| 3 | JobCard.tsx, ProgressBar.tsx |
| 4 | HomeScreen.tsx, JobListScreen.tsx, PlayerScreen.tsx |

### Potential Blockers

| Blocker | Resolution |
|---------|------------|
| Node not installed | `brew install node` |
| Android SDK not configured | Set ANDROID_HOME, install via Android Studio |
| No Android emulator | Create one via Android Studio AVD Manager |
| Metro bundler port in use | Kill process on port 8081 |
| Gradle build fails | Check Java version, may need JDK 17 |

### Pre-Flight Check

```bash
# Verify prerequisites
node --version          # Need 18+
java --version          # Need JDK 17
echo $ANDROID_HOME      # Must be set
adb devices             # Emulator should be listed
```

### Time Estimate

- RN init + deps: ~5 minutes
- TypeScript code: ~10 minutes
- Android build (first time): ~3-5 minutes
- Testing: ~5 minutes
- **Total:** ~25 minutes

---

## Notes

- **Mock data first.** JobListScreen uses hardcoded mock jobs until Stage 7
  integrates with the real API. This lets us build and test UI independently.

- **No upload yet.** HomeScreen picks videos but doesn't upload. The alert shows
  file info, then navigates to JobList. Real upload comes in Stage 7.

- **No camera capture yet.** "Record Video" button is placeholder. Could add
  react-native-camera later if needed.

- **Android only for now.** iOS builds are more complex (Xcode, pods). Focus on
  Android emulator for the demo.

- **PlayerScreen is a stub.** Just shows "Coming soon" until Stage 8B adds
  react-native-video HLS playback.

- **API_URL config.** Points to localhost:8080 for emulator. Android emulator
  uses 10.0.2.2 to reach host machine.
