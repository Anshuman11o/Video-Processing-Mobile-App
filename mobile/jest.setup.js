// Native modules are not linked in the Jest environment, so stub the ones the
// app imports at module load. Without this, importing HomeScreen throws
// "TurboModuleRegistry.getEnforcing(...): 'RNDocumentPicker' could not be found".
jest.mock('@react-native-documents/picker', () => ({
  pick: jest.fn(() => Promise.resolve([])),
  keepLocalCopy: jest.fn(() => Promise.resolve([])),
  types: {video: 'video/*'},
  errorCodes: {OPERATION_CANCELED: 'OPERATION_CANCELED'},
  isErrorWithCode: jest.fn(() => false),
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

// react-native-video renders a native view, so any test that actually mounted
// PlayerScreen would fail on the real component. It currently passes only
// because no test mounts that screen — which is a fact about the test suite,
// not a property worth relying on. Stubbing it keeps the day someone writes
// that test an ordinary day.
//
// SelectedTrackType is a real enum in the library and PlayerScreen reads
// LANGUAGE off it at module scope, so the mock has to supply it or the import
// crashes before any test body runs.
jest.mock('react-native-video', () => ({
  __esModule: true,
  default: 'Video',
  SelectedTrackType: {
    SYSTEM: 'system',
    DISABLED: 'disabled',
    TITLE: 'title',
    LANGUAGE: 'language',
    INDEX: 'index',
  },
}));
