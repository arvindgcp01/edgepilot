"use client";

import { useRouter } from "next/navigation";
import { AppShell } from "@/components/app-shell";
import { Badge, Button, Card, EmptyState, PageHeader } from "@/components/ui";

export default function ReportsPage() {
  const router = useRouter();
  return (
    <AppShell>
      <PageHeader
        title={<span className="flex items-center gap-2">Reports <Badge tone="neutral">Preview</Badge></span>}
        description="Scheduled and on-demand exports — fleet inventory, uptime, access history — for compliance and capacity planning."
      />
      <Card padded={false}>
        <EmptyState
          icon="bar-chart"
          title="No report definitions yet"
          description="The audit log can already be exported as CSV for any date range. Inventory and uptime reports will appear here when reporting is enabled."
          action={<Button onClick={() => router.push("/settings/audit-logs")}>Export audit log</Button>}
        />
      </Card>
    </AppShell>
  );
}
