"use client";

import { Facts, PageHeader, Section } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { getOrganization, listUsers } from "@/lib/api/admin";

export default function BillingPage() {
  const org = useAsync(() => getOrganization().catch(() => null), []);
  const users = useAsync(() => listUsers().catch(() => null), []);
  return (
    <>
      <PageHeader title="Billing" description="This is a self-hosted deployment. There is no subscription, no seat limit, and no invoice." />
      <Section title="Usage" description="For reference when planning capacity.">
        <Facts rows={[
          { label: "Deployment", value: "Self-hosted · single organization" },
          { label: "Devices", value: org.data ? String(org.data.device_count) : "—" },
          { label: "Members", value: users.data ? String(users.data.data.length) : "—" },
          { label: "Limits", value: "None enforced" },
        ]} />
      </Section>
    </>
  );
}
