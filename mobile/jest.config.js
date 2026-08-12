module.exports = {
  preset: '@react-native/jest-preset',
  setupFiles: ['<rootDir>/jest.setup.js'],
  // The RN preset does not transform these packages, which ship untranspiled
  // ESM. Without this, importing the navigator throws "Unexpected token 'export'".
  transformIgnorePatterns: [
    'node_modules/(?!(?:@react-native|react-native|@react-navigation|react-native-screens|react-native-safe-area-context)/)',
  ],
};
