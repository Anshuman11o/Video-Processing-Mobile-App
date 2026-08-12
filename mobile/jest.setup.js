// Native modules are not linked in the Jest environment, so stub the ones the
// app imports at module load. Without this, importing HomeScreen throws
// "TurboModuleRegistry.getEnforcing(...): 'RNDocumentPicker' could not be found".
jest.mock('react-native-document-picker', () => ({
  pick: jest.fn(() => Promise.resolve([])),
  types: {video: 'video/*'},
}));

// react-native-blob-util ships untranspiled ESM AND needs a linked native
// module, so it fails twice over under Jest. It is mocked rather than
// transformed because nothing it does — slicing a file, streaming a PUT — is
// meaningful without a device.
//
// This mock is deliberately inert: it must never make the upload tests look
// like they exercised a real transport. The upload logic is tested against an
// explicit fake in __tests__/uploader.test.ts instead.
jest.mock('react-native-blob-util', () => {
  const notImplemented = name => () => {
    throw new Error(
      `react-native-blob-util.${name} is not available under Jest; ` +
        'test the logic that calls it with an injected fake instead',
    );
  };
  return {
    __esModule: true,
    default: {
      fs: {
        dirs: {CacheDir: '/mock/cache', DocumentDir: '/mock/documents'},
        exists: jest.fn(() => Promise.resolve(false)),
        readFile: jest.fn(() => Promise.resolve('')),
        writeFile: jest.fn(() => Promise.resolve()),
        unlink: jest.fn(() => Promise.resolve()),
        slice: jest.fn(notImplemented('fs.slice')),
      },
      config: jest.fn(notImplemented('config')),
      wrap: jest.fn(path => path),
    },
  };
});
