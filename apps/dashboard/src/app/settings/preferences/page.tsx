"use client";

import { useEffect, useState } from "react";
import { Button, Callout, Field, Icon, PageHeader, Section, Select, Switch, useToast, type IconName } from "@/components/ui";
import { cn } from "@/lib/cn";
import { readTheme, setTheme, type ThemePreference } from "@/lib/theme";

type Prefs = {
  density: "comfortable" | "compact";
  timestamps: "relative" | "absolute";
  clock: "local" | "utc";
  landing: "/" | "/devices" | "/enrollment" | "/settings/audit-logs";
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
          title="Theme"
          description="Light for the office, dark for wall displays in dim rooms. System follows this computer's setting. Applies immediately."
        >
          <ThemePicker />
        </Section>
        <Section
          title="Appearance"
          description="Layout and motion for this browser."
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
                <option value="/">Overview</option>
                <option value="/devices">Devices</option>
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

const THEMES: { value: ThemePreference; label: string; icon: IconName }[] = [
  { value: "light", label: "Light", icon: "sun" },
  { value: "dark", label: "Dark", icon: "moon" },
  { value: "system", label: "System", icon: "monitor" },
];

/** Three preview cards. Applies on click: a theme is judged by looking at it,
 *  and a Save button between the choice and the result only gets in the way. */
function ThemePicker() {
  const [pref, setPref] = useState<ThemePreference>("system");
  useEffect(() => setPref(readTheme()), []);

  return (
    <div role="radiogroup" aria-label="Theme" className="grid gap-3 sm:grid-cols-3">
      {THEMES.map((t) => {
        const selected = pref === t.value;
        return (
          <button
            key={t.value}
            type="button"
            role="radio"
            aria-checked={selected}
            onClick={() => {
              setTheme(t.value);
              setPref(t.value);
            }}
            className={cn(
              "overflow-hidden rounded-xl border text-left transition-all",
              selected ? "border-accent ring-4 ring-accent/10" : "border-surface-border hover:border-surface-border-strong",
            )}
          >
            <ThemePreview kind={t.value} />
            <span className="flex items-center gap-2 border-t border-surface-border px-3 py-2.5 text-sm font-medium text-ink">
              <Icon name={t.icon} size={14} className="text-ink-muted" />
              {t.label}
              {selected && <Icon name="check-circle" size={15} className="ml-auto text-accent" />}
            </span>
          </button>
        );
      })}
    </div>
  );
}

function ThemePreview({ kind }: { kind: ThemePreference }) {
  const pane = (dark: boolean) => (
    <div className={cn("flex h-full flex-1 gap-1.5 p-2.5", dark ? "bg-[#0b0e14]" : "bg-[#f7f8fa]")}>
      <div className={cn("w-1/4 rounded-md", dark ? "bg-[#12161f]" : "bg-white ring-1 ring-black/5")} />
      <div className="flex flex-1 flex-col gap-1.5">
        <div className={cn("h-2 w-1/2 rounded", dark ? "bg-[#2c3746]" : "bg-slate-300")} />
        <div className={cn("flex-1 rounded-md", dark ? "bg-[#12161f]" : "bg-white ring-1 ring-black/5")} />
        <div className="h-2 w-1/3 rounded bg-indigo-500" />
      </div>
    </div>
  );
  return (
    <div className="flex h-24">
      {kind === "light" && pane(false)}
      {kind === "dark" && pane(true)}
      {kind === "system" && (
        <>
          {pane(false)}
          {pane(true)}
        </>
      )}
    </div>
  );
}
