"use client";

import { useRouter } from "next/navigation";
import { Badge, Code, ErrorNote, Facts, PageHeader, Section, Skeleton } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { getOrganization, listUsers } from "@/lib/api/admin";
import { fleetSummary } from "@/lib/api/devices";
import { absoluteTime } from "@/lib/format";

export default function GeneralPage() {
  const router = useRouter();
  const org = useAsync(() => getOrganization(), []);
  const fleet = useAsync(() => fleetSummary().catch(() => null), []);
  const users = useAsync(() => listUsers().catch(() => null), []);

  return (
    <>
      <PageHeader title="General" description="Where things stand, and the few facts about this deployment that do not belong to a more specific section." />
      <ErrorNote error={org.error} onRetry={() => void org.reload()} />
      <div className="space-y-4">
        <Section title="At a glance" description="Live counts, with a shortcut to each area.">
          <div className="grid gap-3 sm:grid-cols-3">
            <Stat label="Devices" value={fleet.data?.total} sub={fleet.data ? `${fleet.data.online} online` : undefined} onClick={() => router.push("/devices")} />
            <Stat label="Members" value={users.data?.data.length} sub={users.data ? `${users.data.data.filter((u) => u.status === "active").length} active` : undefined} onClick={() => router.push("/settings/users")} />
            <Stat label="Awaiting approval" value={fleet.data?.pending_approval} sub="devices" onClick={() => router.push("/?status=pending")} />
          </div>
        </Section>

        <Section title="This deployment" description="Identity of the organization and the server it runs on.">
          {org.data ? (
            <Facts rows={[
              { label: "Organization", value: org.data.name },
              { label: "Organization ID", value: <Code>{org.data.id}</Code> },
              { label: "Slug", value: <Code>{org.data.slug}</Code> },
              { label: "Server", value: <Code>edgepilot.visionify.ai</Code> },
              { label: "Created", value: absoluteTime(org.data.created_at) },
            ]} />
          ) : (
            <div className="space-y-3">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-4 w-full" />)}</div>
          )}
        </Section>

        <Section title="Agent" description="What new devices install, and how.">
          <Facts rows={[
            { label: "Published version", value: <span className="flex items-center gap-2"><Code>2.1.0</Code><Badge tone="success" dot>Current</Badge></span> },
            { label: "Install", value: <Code>curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- &lt;token&gt;</Code> },
            { label: "Upgrade", value: <Code>curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash</Code> },
            { label: "Reports", value: "Heartbeat every 15s · health sample every 30s · inventory every 6h" },
          ]} />
        </Section>
      </div>
    </>
  );
}

function Stat({ label, value, sub, onClick }: { label: string; value?: React.ReactNode; sub?: string; onClick: () => void }) {
  return (
    <button type="button" onClick={onClick} className="rounded-md border border-surface-border bg-surface px-4 py-3 text-left transition-colors hover:border-surface-border-strong hover:bg-surface-hover">
      <div className="text-xs font-medium text-ink-faint">{label}</div>
      <div className="mt-0.5 text-lg font-semibold tabular text-ink">{value ?? <Skeleton className="mt-1 h-5 w-12" />}</div>
      {sub && <div className="text-xs text-ink-muted">{sub}</div>}
    </button>
  );
}
