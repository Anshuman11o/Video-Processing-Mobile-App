// Native modules are not linked in the Jest environment, so stub the ones the
// app imports at module load. Without this, importing HomeScreen throws
// "TurboModuleRegistry.getEnforcing(...): 'RNDocumentPicker' could not be found".
jest.mock('react-native-document-picker', () => ({
  pick: jest.fn(() => Promise.resolve([])),
  types: {video: 'video/*'},
}));
