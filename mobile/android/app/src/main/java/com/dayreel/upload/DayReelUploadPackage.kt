package com.dayreel.upload

import android.util.Log
import com.facebook.react.BaseReactPackage
import com.facebook.react.bridge.NativeModule
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.module.model.ReactModuleInfo
import com.facebook.react.module.model.ReactModuleInfoProvider

/**
 * Registers [DayReelUploadModule] so JavaScript can resolve it.
 *
 * `BaseReactPackage` is the lazy path: `getModule` is only called when JS
 * actually asks for the module by name, so nothing here is instantiated during
 * startup — including in the background process WorkManager spins up to run the
 * worker, where no JavaScript exists to ask.
 *
 * **`isTurboModule = false` is load-bearing and was `true` until it was measured.**
 * Under the New Architecture a Java module declared as a TurboModule is resolved
 * in two halves: `ReactPackageTurboModuleManagerDelegate` finds the Java
 * instance, and then C++ asks `DefaultTurboModuleManagerDelegate::javaModuleProvider`
 * for the generated spec that binds it to JSI. That provider is
 * `autolinking_ModuleProvider` plus an app-level provider generated only from a
 * `codegenConfig` block in `mobile/package.json`. There is no such block, so for
 * `DayReelUpload` the C++ half returned `nullptr`, `TurboModuleRegistry.get`
 * returned null, and `isBackgroundUploadAvailable()` silently reported false —
 * measured on device: `turboRegistryGet=false nativeModulesLookup=false`.
 *
 * Declaring it a legacy module instead routes it through the interop binding,
 * which builds the JSI wrapper by reflecting over `@ReactMethod` and needs no
 * generated spec. That path is live by default in this version —
 * `ReactNativeNewArchitectureFeatureFlagsDefaults.useTurboModuleInterop()` is
 * `true` — and it is how every non-codegen'd native module in the app already
 * works. The cost is that argument types are checked at call time by reflection
 * rather than at compile time, which was already the case here: this module is
 * registered by hand precisely because it has no codegen spec.
 */
class DayReelUploadPackage : BaseReactPackage() {

    override fun getModule(name: String, reactContext: ReactApplicationContext): NativeModule? =
        if (name == DayReelUploadModule.NAME) {
            // The one line that says "JavaScript asked". Its absence is what a
            // silent fallback to the foreground uploader looks like from here.
            Log.i(UploadWorker.TAG, "handing ${DayReelUploadModule.NAME} to JavaScript")
            DayReelUploadModule(reactContext)
        } else {
            null
        }

    override fun getReactModuleInfoProvider(): ReactModuleInfoProvider = ReactModuleInfoProvider {
        mapOf(
            DayReelUploadModule.NAME to
                ReactModuleInfo(
                    name = DayReelUploadModule.NAME,
                    // The class, not the JS name. Nothing on the resolution path
                    // reads this today, which is exactly why it held a wrong
                    // value without any symptom.
                    className = DayReelUploadModule::class.java.name,
                    canOverrideExistingModule = false,
                    // Nothing may touch this module before JS asks for it. The
                    // upload is already running by then, or is not this
                    // module's business.
                    needsEagerInit = false,
                    isCxxModule = false,
                    isTurboModule = false,
                )
        )
    }
}
