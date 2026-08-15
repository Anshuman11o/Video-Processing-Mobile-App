# Mobile App (CaptionClips)

React Native + TypeScript mobile shell for the CaptionClips video processing app.

## Structure

```
mobile/
├── App.tsx                     # Entry point, renders AppNavigator
├── src/
│   ├── types/api.ts            # TypeScript types matching Go backend models
│   ├── api/client.ts           # API client (axios) + mock data
│   ├── screens/
│   │   ├── HomeScreen.tsx      # Video picker with document picker
│   │   ├── JobListScreen.tsx   # FlatList of jobs with status/progress
│   │   └── PlayerScreen.tsx    # HLS player placeholder (Stage 6)
│   └── navigation/
│       └── AppNavigator.tsx    # React Navigation stack navigator
```

## Navigation Flow

Home → (select video) → JobList → (tap completed job) → Player

## Current State (Stage 2B)

- Shell with 3 screens and navigation
- Mock data in api/client.ts (no real API calls yet)
- Video picker integrated (react-native-document-picker)
- Dark theme (#0f0f0f background)
- TypeScript compiles clean

## What's Next

- Stage 7: Real upload integration (presigned URL upload to S3)
- Stage 6: HLS player in PlayerScreen (react-native-video or expo-av)
- Background upload with retry logic
