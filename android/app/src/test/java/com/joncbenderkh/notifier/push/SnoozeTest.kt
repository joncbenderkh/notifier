// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import org.junit.Assert.assertEquals
import org.junit.Test
import java.time.LocalDateTime
import java.time.ZoneId
import java.time.ZonedDateTime
import java.util.Locale

class SnoozeTest {
    private val zone = ZoneId.of("Europe/Berlin")

    private fun at(text: String) = ZonedDateTime.of(LocalDateTime.parse(text), zone).toEpochSecond()

    @Test
    fun putsOffByMinutesOrHours() {
        val now = at("2026-10-16T14:00")
        assertEquals(now + 900, Snooze.QUARTER_HOUR.at(now, zone))
        assertEquals(now + 3600, Snooze.HOUR.at(now, zone))
    }

    @Test
    fun offersAMinuteOnlyInDebugBuilds() {
        assertEquals(listOf(Snooze.QUARTER_HOUR, Snooze.HOUR, Snooze.MORNING), Snooze.choices(debug = false))
        assertEquals(Snooze.MINUTE, Snooze.choices(debug = true).first())
        assertEquals(1_000L + 60, Snooze.MINUTE.at(1_000, zone))
    }

    @Test
    fun theMorningIsNineOClockLocalTime() {
        assertEquals(at("2026-10-17T09:00"), Snooze.MORNING.at(at("2026-10-16T23:30"), zone))
        assertEquals(at("2026-10-17T09:00"), Snooze.MORNING.at(at("2026-10-16T09:00"), zone))
        assertEquals("Tomorrow at 09:00", Snooze.MORNING.label(at("2026-10-16T23:30"), zone, Locale.GERMANY))
        // Until 9:00, this morning's.
        assertEquals(at("2026-10-16T09:00"), Snooze.MORNING.at(at("2026-10-16T00:30"), zone))
        assertEquals(at("2026-10-16T09:00"), Snooze.MORNING.at(at("2026-10-16T07:00"), zone))
        assertEquals("This morning at T", Snooze.MORNING.label(at("2026-10-16T08:59"), zone) { "T" })
        // Across the change to winter time, still 9:00.
        assertEquals(at("2026-10-25T09:00"), Snooze.MORNING.at(at("2026-10-24T20:00"), zone))
    }
}
