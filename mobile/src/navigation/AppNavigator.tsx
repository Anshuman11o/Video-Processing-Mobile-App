import React from 'react';
import {NavigationContainer} from '@react-navigation/native';
import {createNativeStackNavigator} from '@react-navigation/native-stack';

import HomeScreen from '../screens/HomeScreen';
import SelectionScreen from '../screens/SelectionScreen';
import UploadProgressScreen from '../screens/UploadProgressScreen';
import JobListScreen from '../screens/JobListScreen';
import JobDetailScreen from '../screens/JobDetailScreen';
import PlayerScreen from '../screens/PlayerScreen';
import type {PickedClip} from '../types/clips';

/**
 * Home → Selection → UploadProgress → JobList → JobDetail → Player.
 *
 * The player is the last screen rather than the third. It used to be reached
 * straight from a pick, which meant everything between choosing a file and
 * watching the result was invisible: no "before" state to compare against, no
 * upload to watch, and no sign that a four-stage pipeline had run at all.
 *
 * `Selection` and `UploadProgress` carry whole clip objects as params. They are
 * plain serialisable data — see `PickedClip` — and they have nowhere else to
 * live: these clips exist only on the device and have no job id yet.
 */
export type RootStackParamList = {
  Home: undefined;
  Selection: {clips: PickedClip[]};
  UploadProgress: {clips: PickedClip[]};
  JobList: undefined;
  /** `filename` is only for the header while the first poll is in flight. */
  JobDetail: {jobId: string; filename?: string};
  Player: {jobId: string};
};

const Stack = createNativeStackNavigator<RootStackParamList>();

export default function AppNavigator() {
  return (
    <NavigationContainer>
      <Stack.Navigator
        initialRouteName="Home"
        screenOptions={{
          headerStyle: {backgroundColor: '#0f0f0f'},
          headerTintColor: '#ffffff',
          headerTitleStyle: {fontWeight: '600'},
          contentStyle: {backgroundColor: '#0f0f0f'},
        }}>
        <Stack.Screen
          name="Home"
          component={HomeScreen}
          options={{headerShown: false}}
        />
        <Stack.Screen
          name="Selection"
          component={SelectionScreen}
          options={{title: 'Your Clips'}}
        />
        <Stack.Screen
          name="UploadProgress"
          component={UploadProgressScreen}
          options={{title: 'Uploading'}}
        />
        <Stack.Screen
          name="JobList"
          component={JobListScreen}
          options={{title: 'My Jobs'}}
        />
        <Stack.Screen
          name="JobDetail"
          component={JobDetailScreen}
          options={{title: 'Job'}}
        />
        <Stack.Screen
          name="Player"
          component={PlayerScreen}
          options={{title: 'Preview'}}
        />
      </Stack.Navigator>
    </NavigationContainer>
  );
}
