"use client";

import { useRouter } from "next/navigation";
import { Button, Callout, Code, Facts, PageHeader, Section } from "@/components/ui";

/**
 * The bottom of the settings tree. Everything irreversible about this
 * deployment happens on the host, deliberately: a dashboard button that can
 * delete an organization is a button that will one day be clicked by mistake.
 */
export default function AdvancedPage() {
  const router = useRouter();
  return (
    <>
      <PageHeader title="Advanced" description="Data handling and the operations that live on the host rather than in the dashboard." />
      <div className="space-y-4">
        <Section title="Data retention" description="Configured per organization.">
          <p className="text-sm text-ink-muted">Metrics and audit retention are set on the Organization page.</p>
          <div className="flex justify-end"><Button variant="ghost" onClick={() => router.push("/settings/organization")}>Open retention settings</Button></div>
        </Section>
        <Section title="Data export" description="Everything in the database is yours, and PostgreSQL is the export format.">
          <Facts rows={[
            { label: "Full export", value: <Code>docker exec edgepilot-postgres-1 pg_dump -U edgepilot edgepilot</Code> },
            { label: "Excludes", value: "Nothing — but device private keys were never on the server to begin with" },
          ]} />
        </Section>
        <Section tone="danger" title="Delete organization" description="Not available from the dashboard, by design.">
          <Callout tone="danger" title="On the host only">
            Deleting an organization decommissions every device and revokes every certificate. It is an <code className="font-mono">epctl</code> operation performed on the server by someone with shell access, not a button.
          </Callout>
        </Section>
      </div>
    </>
  );
}
