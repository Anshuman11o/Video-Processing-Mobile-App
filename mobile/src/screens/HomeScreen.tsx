import React, {useCallback} from 'react';
import {View, Text, TouchableOpacity, StyleSheet, Alert} from 'react-native';
import {pick} from 'react-native-document-picker';
import type {NativeStackNavigationProp} from '@react-navigation/native-stack';
import type {RootStackParamList} from '../navigation/AppNavigator';

type HomeScreenProps = {
  navigation: NativeStackNavigationProp<RootStackParamList, 'Home'>;
};

export default function HomeScreen({navigation}: HomeScreenProps) {
  const handleSelectVideo = useCallback(async () => {
    try {
      const [result] = await pick({
        type: ['video/*'],
      });

      if (result) {
        Alert.alert(
          'Video Selected',
          `${result.name}\nSize: ${((result.size ?? 0) / 1024 / 1024).toFixed(1)} MB`,
          [
            {text: 'Cancel', style: 'cancel'},
            {
              text: 'Upload',
              onPress: () => {
                // TODO: Stage 7 - actual upload logic
                console.log('Would upload:', result.uri);
                navigation.navigate('JobList');
              },
            },
          ],
        );
      }
    } catch (err: any) {
      if (err?.code !== 'DOCUMENT_PICKER_CANCELED') {
        console.error('Error picking video:', err);
      }
    }
  }, [navigation]);

  return (
    <View style={styles.container}>
      <Text style={styles.title}>DayReel</Text>
      <Text style={styles.subtitle}>Turn your videos into reels</Text>

      <TouchableOpacity style={styles.button} onPress={handleSelectVideo}>
        <Text style={styles.buttonText}>Select Video</Text>
      </TouchableOpacity>

      <TouchableOpacity
        style={styles.secondaryButton}
        onPress={() => navigation.navigate('JobList')}>
        <Text style={styles.secondaryButtonText}>View Jobs</Text>
      </TouchableOpacity>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    justifyContent: 'center',
    alignItems: 'center',
    backgroundColor: '#0f0f0f',
    padding: 24,
  },
  title: {
    fontSize: 36,
    fontWeight: '700',
    color: '#ffffff',
    marginBottom: 8,
  },
  subtitle: {
    fontSize: 16,
    color: '#888888',
    marginBottom: 48,
  },
  button: {
    backgroundColor: '#6366f1',
    paddingHorizontal: 32,
    paddingVertical: 16,
    borderRadius: 12,
    marginBottom: 16,
    width: '100%',
    alignItems: 'center',
  },
  buttonText: {
    color: '#ffffff',
    fontSize: 18,
    fontWeight: '600',
  },
  secondaryButton: {
    paddingHorizontal: 32,
    paddingVertical: 16,
    borderRadius: 12,
    borderWidth: 1,
    borderColor: '#333333',
    width: '100%',
    alignItems: 'center',
  },
  secondaryButtonText: {
    color: '#888888',
    fontSize: 16,
  },
});
