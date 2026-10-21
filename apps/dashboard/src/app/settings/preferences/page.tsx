"use client";

import { useEffect, useState } from "react";
import { Button, Callout, Field, PageHeader, Section, Select, Switch, useToast } from "@/components/ui";

type Prefs = {
  density: "comfortable" | "compact";
  timestamps: "relative" | "absolute";
  clock: "local" | "utc";
  landing: "/" | "/enrollment" | "/settings/audit-logs";
  page_size: 10 | 25 | 50;
  reduce_motion: boolean;
};

const DEFAULTS: Prefs = { density: "comfortable", timestamps: "relative", clock: "local", landing: "/", page_size: 25, reduce_motion: false };
const KEY = "edgepilot.preferences";

/**
 * Per-browser display preferences. These never reach the server: they describe
 * this screen, not this account, and an operator's wall display and laptop
 * legitimately want different answers.
 */
export default function PreferencesPage() {
  const { push } = useToast();
  const [saved, setSaved] = useState<Prefs>(DEFAULTS);
  const [draft, setDraft] = useState<Prefs>(DEFAULTS);

  useEffect(() => {
    try {
      const raw = window.localStorage.getItem(KEY);
      if (raw) {
        const parsed = { ...DEFAULTS, ...(JSON.parse(raw) as Partial<Prefs>) };
        setSaved(parsed);
        setDraft(parsed);
      }
    } catch {
      // Storage unavailable or corrupt: defaults are fine.
    }
  }, []);

  const dirty = JSON.stringify(draft) !== JSON.stringify(saved);

  function save() {
    try {
      window.localStorage.setItem(KEY, JSON.stringify(draft));
    } catch {
      // Private mode. The preference holds for this page load only.
    }
    setSaved(draft);
    document.documentElement.dataset.density = draft.density;
    push({ title: "Preferences saved", description: "Applies to this browser only.", tone: "success" });
  }

  return (
    <>
      <PageHeader title="Preferences" description="How this browser displays EdgePilot. Stored locally; not shared with other devices or members." />
      <div className="space-y-4">
        <Section
          title="Appearance"
          description="EdgePilot is dark by design — it is watched on wall displays in dim rooms. A light theme is not offered."
          footer={
            <>
              {dirty && <Button variant="ghost" onClick={() => setDraft(saved)}>Discard</Button>}
              <Button variant="primary" disabled={!dirty} onClick={save}>Save preferences</Button>
            </>
          }
        >
          <Field label="Density" htmlFor="density" hint="Compact tightens table rows for large fleets.">
            <Select id="density" value={draft.density} onChange={(e) => setDraft({ ...draft, density: e.target.value as Prefs["density"] })}>
              <option value="comfortable">Comfortable</option>
              <option value="compact">Compact</option>
            </Select>
          </Field>
          <Switch
            checked={draft.reduce_motion}
            onChange={(v) => setDraft({ ...draft, reduce_motion: v })}
            label="Reduce motion"
            description="Disables drawer and toast animations."
          />
        </Section>

        <Section
          title="Time and tables"
          description="Timestamps in tables can be relative (“3m ago”) or absolute. Hovering always shows the other."
          footer={<Button variant="primary" disabled={!dirty} onClick={save}>Save preferences</Button>}
        >
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Timestamps" htmlFor="ts">
              <Select id="ts" value={draft.timestamps} onChange={(e) => setDraft({ ...draft, timestamps: e.target.value as Prefs["timestamps"] })}>
                <option value="relative">Relative</option>
                <option value="absolute">Absolute</option>
              </Select>
            </Field>
            <Field label="Clock" htmlFor="clock">
              <Select id="clock" value={draft.clock} onChange={(e) => setDraft({ ...draft, clock: e.target.value as Prefs["clock"] })}>
                <option value="local">Local time</option>
                <option value="utc">UTC</option>
              </Select>
            </Field>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Rows per page" htmlFor="rows">
              <Select id="rows" value={draft.page_size} onChange={(e) => setDraft({ ...draft, page_size: Number(e.target.value) as Prefs["page_size"] })}>
                <option value={10}>10</option>
                <option value={25}>25</option>
                <option value={50}>50</option>
              </Select>
            </Field>
            <Field label="Start on" htmlFor="landing" hint="The page that opens after sign-in.">
              <Select id="landing" value={draft.landing} onChange={(e) => setDraft({ ...draft, landing: e.target.value as Prefs["landing"] })}>
                <option value="/">Fleet</option>
                <option value="/enrollment">Enrollment</option>
                <option value="/settings/audit-logs">Audit logs</option>
              </Select>
            </Field>
          </div>
          <Callout tone="info">Timestamp and clock preferences are read by new pages as they load; open pages update on their next refresh.</Callout>
        </Section>
      </div>
    </>
  );
}
