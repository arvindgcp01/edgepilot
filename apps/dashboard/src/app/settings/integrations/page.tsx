"use client";

import { Callout, Card, Code, EmptyState, Facts, PageHeader, Section } from "@/components/ui";

export default function IntegrationsPage() {
  return (
    <>
      <PageHeader title="API / Integrations" description="Programmatic access to the organization." />
      <div className="space-y-4">
        <Section title="API" description="The same REST API the dashboard uses, with the same permissions.">
          <Facts rows={[
            { label: "Base URL", value: <Code>https://edgepilot.visionify.ai/api/v1</Code> },
            { label: "Authentication", value: "Bearer access token from POST /auth/login, refreshed via POST /auth/refresh" },
            { label: "Specification", value: <Code>packages/shared/openapi/openapi.yaml</Code> },
            { label: "Live events", value: <Code>GET /api/v1/ws?access_token=… · subscribe to fleet, device:&lt;id&gt;</Code> },
          ]} />
          <Callout tone="info">Calls are made as a user. Long-lived API keys with their own scopes are not available on this server yet — use a dedicated service account.</Callout>
        </Section>
        <Card padded={false}>
          <EmptyState icon="link" title="No webhooks or connected services" description="Outbound webhooks and third-party integrations need the alert engine, which the server does not have yet." />
        </Card>
      </div>
    </>
  );
}
