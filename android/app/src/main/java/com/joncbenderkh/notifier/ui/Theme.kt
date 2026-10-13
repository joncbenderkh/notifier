// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

// The OpenDesign mockup's tokens (issue #44): a restrained green on cool
// neutrals. Always applied, rather than the wallpaper's dynamic colours,
// so the app looks the same on every phone.

private val LightColors = run {
    val primary = Color(0xFF15461A)
    val tonal = Color(0xFFE1F0E1)
    val background = Color(0xFFF6F9FC)
    val foreground = Color(0xFF121C23)
    val muted = Color(0xFF5A656D)
    val soft = Color(0xFFEAEFF3)
    lightColorScheme(
        primary = primary,
        onPrimary = Color(0xFFFAFDFA),
        primaryContainer = tonal,
        onPrimaryContainer = foreground,
        secondary = primary,
        onSecondary = Color(0xFFFAFDFA),
        secondaryContainer = tonal,
        onSecondaryContainer = foreground,
        tertiary = primary,
        background = background,
        onBackground = foreground,
        surface = background,
        onSurface = foreground,
        surfaceVariant = soft,
        onSurfaceVariant = muted,
        surfaceContainerLowest = Color.White,
        surfaceContainerLow = soft,
        surfaceContainer = soft,
        surfaceContainerHigh = soft,
        surfaceContainerHighest = soft,
        outline = muted,
        outlineVariant = Color(0xFFD9DFE3),
        error = Color(0xFF94151D),
        onError = Color.White,
    )
}

private val DarkColors = run {
    val primary = Color(0xFFA0D4A0)
    val onPrimary = Color(0xFF142114)
    val tonal = Color(0xFF283828)
    val background = Color(0xFF11171B)
    val foreground = Color(0xFFE8ECEF)
    val muted = Color(0xFFA0A9B0)
    val soft = Color(0xFF1E252B)
    val raised = Color(0xFF20272D)
    darkColorScheme(
        primary = primary,
        onPrimary = onPrimary,
        primaryContainer = tonal,
        onPrimaryContainer = foreground,
        secondary = primary,
        onSecondary = onPrimary,
        secondaryContainer = tonal,
        onSecondaryContainer = foreground,
        tertiary = primary,
        background = background,
        onBackground = foreground,
        surface = background,
        onSurface = foreground,
        surfaceVariant = soft,
        onSurfaceVariant = muted,
        surfaceContainerLowest = background,
        surfaceContainerLow = soft,
        surfaceContainer = soft,
        surfaceContainerHigh = raised,
        surfaceContainerHighest = raised,
        outline = muted,
        outlineVariant = Color(0xFF383E43),
        error = Color(0xFFF8A49D),
        onError = Color(0xFF2B0A08),
    )
}

@Composable
fun NotifierTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = if (isSystemInDarkTheme()) DarkColors else LightColors, content = content)
}
