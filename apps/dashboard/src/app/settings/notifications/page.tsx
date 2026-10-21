"use client";

import { Card, EmptyState, PageHeader } from "@/components/ui";

export default function NotificationsPage() {
  return (
    <>
      <PageHeader title="Notifications" description="Which events the organization is told about, and where." />
      <Card padded={false}>
        <EmptyState
          icon="bell"
          title="No notification channels on this server"
          description="Email, Slack and webhook delivery need an alert engine the server does not have yet. Device state is visible live on the Fleet page in the meantime."
        />
      </Card>
    </>
  );
}
