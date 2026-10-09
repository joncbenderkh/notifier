plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

// Firebase client config is supplied at build time instead of through a
// committed google-services.json: set these as Gradle properties (for example
// in ~/.gradle/gradle.properties) or as NOTIFIER_FIREBASE_* environment
// variables. Without them the app builds but cannot pair.
fun firebaseConfig(name: String): String =
    providers.gradleProperty("notifier.firebase.$name")
        .orElse(providers.environmentVariable("NOTIFIER_FIREBASE_${name.uppercase()}"))
        .getOrElse("")

android {
    namespace = "com.joncbenderkh.notifier"
    compileSdk {
        version = release(37) { minorApiLevel = 2 }
    }

    defaultConfig {
        applicationId = "com.joncbenderkh.notifier"
        minSdk = 26
        targetSdk = 37
        versionCode = 1
        versionName = "0.1.0"

        for (name in listOf("projectId", "appId", "apiKey", "senderId")) {
            buildConfigField("String", "FIREBASE_${name.uppercase()}", "\"${firebaseConfig(name)}\"")
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
            // No signingConfig until a release keystore exists (see CLAUDE.md).
            // The release workflow builds this as an unsigned artifact until then.
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }
}

dependencies {
    implementation(libs.core.ktx)
    implementation(libs.lifecycle.runtime.compose)
    implementation(libs.lifecycle.viewmodel.compose)
    implementation(libs.activity.compose)
    implementation(platform(libs.compose.bom))
    implementation(libs.ui)
    implementation(libs.ui.tooling.preview)
    implementation(libs.material3)
    implementation(platform(libs.firebase.bom))
    implementation(libs.firebase.messaging)
    implementation(libs.coroutines.play.services)
    implementation(libs.serialization.json)
    implementation(libs.okhttp)

    testImplementation(libs.junit)
    debugImplementation(libs.ui.tooling)
}
