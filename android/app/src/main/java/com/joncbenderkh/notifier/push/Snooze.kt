// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import com.joncbenderkh.notifier.BuildConfig
import java.time.Instant
import java.time.LocalTime
import java.time.ZoneId
import java.time.ZonedDateTime
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle
import java.util.Locale

/** How long a snoozed message waits to be shown again. */
enum class Snooze {
    /** For trying reminders out; debug builds only ([choices]). */
    MINUTE,
    QUARTER_HOUR,
    HOUR,
    MORNING,
    ;

    /**
     * The choice as offered at [now] (Unix seconds) in [zone], with times
     * as [locale] writes them, or as [time] writes a Unix time.
     */
    fun label(
        now: Long,
        zone: ZoneId,
        locale: Locale = Locale.getDefault(),
        time: (Long) -> String = { NINE.format(DateTimeFormatter.ofLocalizedTime(FormatStyle.SHORT).withLocale(locale)) },
    ): String = when (this) {
        MINUTE -> "In 1 minute (debug build)"
        QUARTER_HOUR -> "In 15 minutes"
        HOUR -> "In 1 hour"
        MORNING -> {
            val nine = time(at(now, zone))
            if (beforeNine(local(now, zone))) "This morning at $nine" else "Tomorrow at $nine"
        }
    }

    /** When a message snoozed at [now] (Unix seconds) shows again, in [zone] for the morning. */
    fun at(now: Long, zone: ZoneId): Long = when (this) {
        MINUTE -> now + 60
        QUARTER_HOUR -> now + 15 * 60
        HOUR -> now + 60 * 60
        // At 9:00 local time, even across a daylight saving change: this
        // morning's until then.
        MORNING -> local(now, zone).let { t ->
            (if (beforeNine(t)) t.toLocalDate() else t.toLocalDate().plusDays(1))
                .atTime(NINE).atZone(zone).toEpochSecond()
        }
    }

    companion object {
        /** The choices to offer: [MINUTE] only in a debug build. */
        fun choices(debug: Boolean = BuildConfig.DEBUG) = entries.filter { it != MINUTE || debug }

        private fun local(now: Long, zone: ZoneId): ZonedDateTime = Instant.ofEpochSecond(now).atZone(zone)

        private val NINE = LocalTime.of(9, 0)

        private fun beforeNine(t: ZonedDateTime) = t.toLocalTime() < NINE
    }
}
