// SPDX-License-Identifier: GPL-3.0-or-later
import { describe, expect, it } from "vitest";
import { FORMATS, isFormat, PayloadError } from "../src/hooks";
import { MAX_BODY_BYTES, MAX_TITLE_BYTES, utf8Length } from "../src/text";

const { alertmanager, grafana, generic } = FORMATS;

// Shaped as in Alertmanager's webhook_config documentation (version 4).
const alertmanagerPayload = {
  version: "4",
  groupKey: '{}:{alertname="HostDown"}',
  truncatedAlerts: 0,
  status: "firing",
  receiver: "notifier",
  groupLabels: { alertname: "HostDown" },
  commonLabels: { alertname: "HostDown", job: "node" },
  commonAnnotations: {},
  externalURL: "http://alertmanager:9093",
  alerts: [
    {
      status: "firing",
      labels: { alertname: "HostDown", instance: "nas-01:9100", severity: "warning" },
      annotations: { summary: "nas-01 is down", description: "No scrape for 5 minutes." },
      startsAt: "2026-10-13T10:00:00Z",
      endsAt: "0001-01-01T00:00:00Z",
      generatorURL: "http://prometheus:9090/graph",
      fingerprint: "a1b2c3",
    },
    {
      status: "firing",
      labels: { alertname: "HostDown", instance: "pi-02:9100", severity: "critical" },
      annotations: { description: "No scrape for 5 minutes." },
      fingerprint: "d4e5f6",
    },
  ],
};

// Grafana's webhook contact point adds its own fields to the same shape.
const grafanaPayload = {
  ...alertmanagerPayload,
  orgId: 1,
  state: "alerting",
  title: "[FIRING:2] HostDown (node)",
  message: "**Firing**\n\nValue: A=0\nLabels:\n - alertname = HostDown\n...",
};

describe("alertmanager", () => {
  it("summarises firing alerts, one line each", () => {
    expect(alertmanager(alertmanagerPayload)).toEqual({
      title: "[FIRING:2] HostDown",
      body: "nas-01 is down (nas-01:9100)\nNo scrape for 5 minutes. (pi-02:9100)",
      priority: "high",
    });
  });

  it("pages only for urgent severities, and never for a resolution", () => {
    const warning = { ...alertmanagerPayload, alerts: [alertmanagerPayload.alerts[0]] };
    expect(alertmanager(warning).priority).toBe("default");
    const resolved = {
      ...alertmanagerPayload,
      status: "resolved",
      alerts: alertmanagerPayload.alerts.map((a) => ({ ...a, status: "resolved" })),
    };
    expect(alertmanager(resolved)).toMatchObject({ title: "[RESOLVED] HostDown", priority: "default" });
  });

  it("marks resolved alerts in a firing group", () => {
    const mixed = { ...alertmanagerPayload, alerts: [alertmanagerPayload.alerts[0], { ...alertmanagerPayload.alerts[1], status: "resolved" }] };
    expect(alertmanager(mixed)).toMatchObject({
      title: "[FIRING:1] HostDown",
      body: "nas-01 is down (nas-01:9100)\nResolved: No scrape for 5 minutes. (pi-02:9100)",
      priority: "default",
    });
  });

  it("names a mixed group by what it has in common", () => {
    const mixed = {
      status: "firing",
      alerts: [
        { status: "firing", labels: { alertname: "A" } },
        { status: "firing", labels: { alertname: "B" } },
      ],
    };
    expect(alertmanager(mixed)).toMatchObject({ title: "[FIRING:2] alerts", body: "A\nB" });
  });

  it("cuts long groups, counting what Alertmanager truncated", () => {
    const alerts = Array.from({ length: 500 }, (_, i) => ({
      status: "firing",
      labels: { alertname: "HostDown", instance: `host-${i}` },
      annotations: { summary: "Host is down" },
    }));
    const m = alertmanager({ ...alertmanagerPayload, truncatedAlerts: 7, alerts });
    expect(m.title).toBe("[FIRING:507] HostDown");
    expect(utf8Length(m.body)).toBeLessThanOrEqual(MAX_BODY_BYTES);
    const lines = m.body.split("\n");
    const kept = lines.length - 1;
    expect(lines.at(-1)).toBe(`and ${500 - kept + 7} more`);
  });

  it("cleans what the tool sends", () => {
    const m = alertmanager({
      status: "firing",
      groupLabels: { alertname: "Evil‮eman" },
      alerts: [{ status: "firing", annotations: { summary: "\u001b[31mred\u001b[0m\r\nline\u0007" } }],
    });
    expect(m.title).toBe("[FIRING:1] Evil�eman");
    expect(m.body).toBe("red line�");
  });

  it("ignores unknown fields and rejects what is not an alert", () => {
    expect(alertmanager({ ...alertmanagerPayload, newField: { x: 1 } }).title).toBe("[FIRING:2] HostDown");
    for (const bad of [null, [], "x", {}, { alerts: [] }, { alerts: "x" }, { status: "firing", alerts: [null] }, { alerts: [42, "x"] }]) {
      expect(() => alertmanager(bad)).toThrow(PayloadError);
    }
  });
});

describe("grafana", () => {
  it("uses Grafana's title and the alert lines, not its long message", () => {
    expect(grafana(grafanaPayload)).toEqual({
      title: "[FIRING:2] HostDown (node)",
      body: "nas-01 is down (nas-01:9100)\nNo scrape for 5 minutes. (pi-02:9100)",
      priority: "high",
    });
    expect(grafana({ ...grafanaPayload, title: undefined }).title).toBe("[FIRING:2] HostDown");
  });
});

describe("generic", () => {
  it("takes a title, body and priority", () => {
    expect(generic({ title: "Deploy done", body: "v1.2.3", priority: "high", source: "ci" })).toEqual({
      title: "Deploy done",
      body: "v1.2.3",
      priority: "high",
    });
    expect(generic({ title: "Hi" })).toEqual({ title: "Hi", body: "", priority: "default" });
  });

  it("cuts and cleans rather than rejecting", () => {
    const m = generic({ title: "t\n".repeat(400), body: "b".repeat(5000) });
    expect(m.title).not.toContain("\n");
    expect(utf8Length(m.title)).toBeLessThanOrEqual(MAX_TITLE_BYTES);
    expect(utf8Length(m.body)).toBeLessThanOrEqual(MAX_BODY_BYTES);
    expect(m.body.endsWith("…")).toBe(true);
  });

  it("keeps text that follows a long run of junk", () => {
    expect(generic({ title: "\u001b[0m".repeat(300) + "Disk full", body: "\n".repeat(8200) + "Filesystem / is full" })).toEqual({
      title: "Disk full",
      body: "Filesystem / is full",
      priority: "default",
    });
  });

  it("cleans a 1 MiB body of junk quickly", () => {
    const start = performance.now();
    for (const junk of ["\u001b[0m".repeat(1 << 18), "\n".repeat((1 << 20) - 1) + "x", "\u001b]".repeat(1 << 19), "\r".repeat(1 << 20)]) {
      generic({ title: "x", body: junk });
    }
    expect(performance.now() - start).toBeLessThan(2000);
  });

  it("reads whitespace-heavy text in linear time", () => {
    const start = performance.now();
    const m = generic({ title: " ".repeat(500_000) + "x", body: " ".repeat(500_000) });
    expect(m.title).toBe("x");
    expect(performance.now() - start).toBeLessThan(500);
  });

  it("rejects a missing title or an unknown priority", () => {
    for (const bad of [null, {}, { title: " " }, { title: 1 }, { title: "x", priority: "urgent" }]) {
      expect(() => generic(bad)).toThrow(PayloadError);
    }
  });
});

it("knows only its own formats", () => {
  expect(["alertmanager", "grafana", "generic"].every(isFormat)).toBe(true);
  for (const f of ["toString", "__proto__", "constructor", "nagios", 1]) expect(isFormat(f)).toBe(false);
});
