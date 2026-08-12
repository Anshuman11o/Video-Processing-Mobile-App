package com.dayreel.upload

import com.facebook.react.BaseReactPackage
import com.facebook.react.bridge.NativeModule
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.module.model.ReactModuleInfo
import com.facebook.react.module.model.ReactModuleInfoProvider

/**
 * Registers [DayReelUploadModule] with the New Architecture's TurboModule
 * registry.
 *
 * `BaseReactPackage` is the lazy path: `getModule` is only called when JS
 * actually asks for the module by name, so nothing here is instantiated during
 * startup — including in the background process WorkManager spins up to run the
 * worker, where no JavaScript exists to ask.
 */
class DayReelUploadPackage : BaseReactPackage() {

    override fun getModule(name: String, reactContext: ReactApplicationContext): NativeModule? =
        if (name == DayReelUploadModule.NAME) DayReelUploadModule(reactContext) else null

    override fun getReactModuleInfoProvider(): ReactModuleInfoProvider = ReactModuleInfoProvider {
        mapOf(
            DayReelUploadModule.NAME to
                ReactModuleInfo(
                    name = DayReelUploadModule.NAME,
                    className = DayReelUploadModule.NAME,
                    canOverrideExistingModule = false,
                    // Nothing may touch this module before JS asks for it. The
                    // upload is already running by then, or is not this
                    // module's business.
                    needsEagerInit = false,
                    isCxxModule = false,
                    isTurboModule = true,
                )
        )
    }
}
