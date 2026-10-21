"use client";

import { useEffect, useState } from "react";
import { Avatar, Badge, Button, Callout, Code, ErrorNote, Field, Input, PageHeader, Section, Skeleton, Switch, useToast } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { getOrganization, updateOrganization, type Organization, type OrgSettings } from "@/lib/api/admin";
import { can } from "@/lib/api/client";
import { absoluteTime } from "@/lib/format";

export default function OrganizationPage() {
  const org = useAsync(() => getOrganization(), []);
  return (
    <>
      <PageHeader title="Organization" description="Identity, and the defaults every device and command inherits. Everything here is stored on the server." />
      <ErrorNote error={org.error} onRetry={() => void org.reload()} />
      <div className="space-y-4">
        {org.data ? <ProfileSection org={org.data} onSaved={org.setData} /> : <SectionSkeleton />}
        {org.data ? <DevicesSection org={org.data} onSaved={org.setData} /> : <SectionSkeleton />}
        {org.data ? <RetentionSection org={org.data} onSaved={org.setData} /> : <SectionSkeleton />}
      </div>
    </>
  );
}

function SectionSkeleton() {
  return (
    <div className="rounded-lg border border-surface-border bg-surface-raised p-5" role="status" aria-label="Loading">
      <div className="grid gap-6 md:grid-cols-[minmax(0,14rem)_1fr]">
        <div className="space-y-2"><Skeleton className="h-4 w-32" /><Skeleton className="h-3 w-48" /></div>
        <div className="space-y-4">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-8 w-full" />)}</div>
      </div>
    </div>
  );
}

function useSave(onSaved: (o: Organization) => void) {
  const { push } = useToast();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  async function save(patch: { name?: string; settings?: OrgSettings }, title: string) {
    setSaving(true);
    setError(null);
    try {
      onSaved(await updateOrganization(patch));
      push({ title, tone: "success" });
    } catch (err) {
      setError(err);
    } finally {
      setSaving(false);
    }
  }
  return { save, saving, error };
}

function ProfileSection({ org, onSaved }: { org: Organization; onSaved: (o: Organization) => void }) {
  const [name, setName] = useState(org.name);
  useEffect(() => setName(org.name), [org.name]);
  const { save, saving, error } = useSave(onSaved);
  const dirty = name.trim() !== org.name && name.trim().length > 0;
  const editable = can("org:write");

  return (
    <Section
      title="Organization profile"
      description="How the organization appears to its members and in the audit log."
      footer={editable && (
        <>
          {dirty && <Button variant="ghost" onClick={() => setName(org.name)} disabled={saving}>Discard</Button>}
          <Button variant="primary" disabled={!dirty} loading={saving} onClick={() => void save({ name: name.trim() }, "Organization renamed")}>Save changes</Button>
        </>
      )}
    >
      <div className="flex items-center gap-4">
        <Avatar name={org.name} size="lg" className="rounded-md" />
        <div>
          <div className="text-sm font-medium text-ink">{org.name}</div>
          <div className="text-xs text-ink-faint">{org.device_count} {org.device_count === 1 ? "device" : "devices"} · created {absoluteTime(org.created_at)}</div>
        </div>
        <Badge tone={org.status === "active" ? "success" : "warning"} className="ml-auto">{org.status}</Badge>
      </div>
      <Field label="Organization name" htmlFor="org-name">
        <Input id="org-name" value={name} maxLength={200} disabled={!editable} onChange={(e) => setName(e.target.value)} />
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Organization ID" hint="Used in API calls and support requests.">
          <div className="flex h-8 items-center"><Code className="text-ink">{org.id}</Code></div>
        </Field>
        <Field label="Slug" hint="Names the tenant at sign-in when a server hosts several.">
          <div className="flex h-8 items-center"><Code className="text-ink">{org.slug}</Code></div>
        </Field>
      </div>
      <ErrorNote error={error} />
    </Section>
  );
}

function DevicesSection({ org, onSaved }: { org: Organization; onSaved: (o: Organization) => void }) {
  const [draft, setDraft] = useState<OrgSettings>(org.settings);
  useEffect(() => setDraft(org.settings), [org.settings]);
  const { save, saving, error } = useSave(onSaved);
  const dirty = Boolean(draft.auto_approve_enrollment) !== Boolean(org.settings.auto_approve_enrollment) ||
    Boolean(draft.require_approval_for_destructive) !== Boolean(org.settings.require_approval_for_destructive);
  const editable = can("org:write");

  return (
    <Section
      title="Device management"
      description="Defaults for how Macs join the fleet and what needs a second pair of eyes."
      footer={editable && (
        <>
          {dirty && <Button variant="ghost" onClick={() => setDraft(org.settings)} disabled={saving}>Discard</Button>}
          <Button variant="primary" disabled={!dirty} loading={saving} onClick={() => void save({ settings: { ...org.settings, auto_approve_enrollment: draft.auto_approve_enrollment, require_approval_for_destructive: draft.require_approval_for_destructive } }, "Device defaults saved")}>Save changes</Button>
        </>
      )}
    >
      <Switch
        checked={Boolean(draft.auto_approve_enrollment)}
        disabled={!editable}
        onChange={(v) => setDraft({ ...draft, auto_approve_enrollment: v })}
        label="Approve enrollments automatically"
        description="A device that presents a valid token receives policy the moment it enrolls. Off means it waits, inert, for an administrator."
      />
      {draft.auto_approve_enrollment && (
        <Callout tone="warning">Any machine holding a valid token joins the fleet — and receives operator SSH keys — without anyone looking at it. Pair with single-use, short-lived tokens.</Callout>
      )}
      <Switch
        checked={Boolean(draft.require_approval_for_destructive)}
        disabled={!editable}
        onChange={(v) => setDraft({ ...draft, require_approval_for_destructive: v })}
        label="Require approval for destructive commands"
        description="Extends the two-person rule from custom scripts to built-in restart and shutdown."
      />
      <p className="text-xs text-ink-faint">The offline threshold (5 minutes) and metrics interval (30 seconds) are server configuration, set by environment on the host.</p>
      <ErrorNote error={error} />
    </Section>
  );
}

function RetentionSection({ org, onSaved }: { org: Organization; onSaved: (o: Organization) => void }) {
  const [metrics, setMetrics] = useState(String(org.settings.metrics_retention_days ?? 0));
  const [audit, setAudit] = useState(String(org.settings.audit_retention_days ?? 0));
  useEffect(() => { setMetrics(String(org.settings.metrics_retention_days ?? 0)); setAudit(String(org.settings.audit_retention_days ?? 0)); }, [org.settings]);
  const { save, saving, error } = useSave(onSaved);
  const dirty = Number(metrics) !== (org.settings.metrics_retention_days ?? 0) || Number(audit) !== (org.settings.audit_retention_days ?? 0);
  const editable = can("org:write");

  return (
    <Section
      title="Data retention"
      description="How long readings and audit entries are kept. Zero uses the server default: 90 days of metrics, 400 days of audit."
      footer={editable && (
        <Button variant="primary" disabled={!dirty} loading={saving} onClick={() => void save({ settings: { ...org.settings, metrics_retention_days: Number(metrics) || 0, audit_retention_days: Number(audit) || 0 } }, "Retention saved")}>Save changes</Button>
      )}
    >
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Metrics retention" htmlFor="ret-metrics" hint="Days of raw device readings.">
          <Input id="ret-metrics" type="number" min={0} max={3650} value={metrics} disabled={!editable} onChange={(e) => setMetrics(e.target.value)} />
        </Field>
        <Field label="Audit retention" htmlFor="ret-audit" hint="Days. Regulated industries typically require 365 or more.">
          <Input id="ret-audit" type="number" min={0} max={3650} value={audit} disabled={!editable} onChange={(e) => setAudit(e.target.value)} />
        </Field>
      </div>
      <ErrorNote error={error} />
    </Section>
  );
}
