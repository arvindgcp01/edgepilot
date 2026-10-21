"use client";

import { useRouter } from "next/navigation";
import { Badge, Button, Facts, PageHeader, Section } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { listUsers } from "@/lib/api/admin";

export default function SecurityPage() {
  const router = useRouter();
  const users = useAsync(() => listUsers().catch(() => null), []);
  const all = users.data?.data ?? [];
  const withoutMFA = all.filter((u) => u.status === "active" && !u.mfa_enabled);

  return (
    <>
      <PageHeader title="Security" description="The boundaries the product enforces, and where the organization stands against them." />
      <div className="space-y-4">
        <Section
          title="Multi-factor authentication"
          description="Enrolled per user from their Profile. There is no organization-wide requirement setting on this server yet."
          footer={<Button variant="ghost" onClick={() => router.push("/settings/users?mfa=off")}>Users without MFA</Button>}
        >
          <div className="flex items-center gap-3">
            <div className="text-2xl font-semibold tabular text-ink">{users.data ? all.filter((u) => u.mfa_enabled).length : "—"}<span className="text-base text-ink-faint"> / {users.data ? all.length : "—"}</span></div>
            <div className="text-sm text-ink-muted">members enrolled</div>
            {users.data && withoutMFA.length > 0 && <Badge tone="warning" className="ml-auto">{withoutMFA.length} active without MFA</Badge>}
          </div>
        </Section>

        <Section title="Device plane" description="How devices authenticate to the server. None of this is configurable; it is how the product works.">
          <Facts rows={[
            { label: "Transport", value: "Mutual TLS 1.3 to the device plane on port 9443" },
            { label: "Identity", value: "Per-device certificate from the organization CA, 90-day life, renewed at two thirds" },
            { label: "Revocation", value: "Checked during every handshake; takes effect within 60 seconds" },
            { label: "Private keys", value: "Generated on the Mac and never leave it" },
          ]} />
        </Section>

        <Section title="Operator access" description="How people reach devices.">
          <Facts rows={[
            { label: "SSH", value: "Through the bastion on port 2222; keys registered per operator, installed by the agent" },
            { label: "Two gates", value: "The bastion checks the key against the database; the Mac's own sshd checks it again" },
            { label: "Browser terminal", value: "Recorded, relayed over the same tunnel" },
            { label: "Network", value: "No IP allowlist on this server" },
          ]} />
        </Section>
      </div>
    </>
  );
}
