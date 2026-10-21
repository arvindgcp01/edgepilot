"use client";

import { Callout, Card, EmptyState, PageHeader } from "@/components/ui";

/**
 * The audit log has a table (`audit_logs`, hash-chained, partitioned) and a
 * permission (`audit:read`). What it does not have yet is a writer or a read
 * endpoint on the server, so the honest page is this one: what will be here,
 * and where the equivalent record lives today.
 */
export default function AuditLogsPage() {
  return (
    <>
      <PageHeader title="Audit Logs" description="Every administrative action, sign-in, and device event — hash-chained, immutable, retained per the organization's policy." />
      <Card padded={false}>
        <EmptyState
          icon="file-text"
          title="Audit logging is not enabled on this server"
          description="The schema and permissions exist; the server does not write entries or serve them yet. Until it does, the server's structured log carries the same events: sign-ins, device approvals, SSH sessions, and user changes."
        />
      </Card>
      <Callout tone="info" className="mt-4" title="Where to look today">
        On the host: <code className="font-mono">docker logs edgepilot-server-1</code>. SSH sessions are additionally recorded per device under the device page.
      </Callout>
    </>
  );
}
