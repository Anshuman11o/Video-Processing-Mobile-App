import React from 'react';
import {NavigationContainer} from '@react-navigation/native';
import {createNativeStackNavigator} from '@react-navigation/native-stack';

import HomeScreen from '../screens/HomeScreen';
import JobListScreen from '../screens/JobListScreen';
import PlayerScreen from '../screens/PlayerScreen';

export type RootStackParamList = {
  Home: undefined;
  JobList: undefined;
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
          name="JobList"
          component={JobListScreen}
          options={{title: 'My Jobs'}}
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
