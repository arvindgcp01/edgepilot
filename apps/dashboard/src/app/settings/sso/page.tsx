"use client";

import { useRouter } from "next/navigation";
import { Badge, Button, Card, Facts, PageHeader, Section } from "@/components/ui";

export default function SSOPage() {
  const router = useRouter();
  return (
    <>
      <PageHeader title="SSO / Authentication" description="How members prove who they are." />
      <div className="space-y-4">
        <Section title="Identity provider" description="SAML or OIDC sign-in through an external provider.">
          <Card className="flex items-center justify-between p-4">
            <div>
              <div className="text-sm text-ink">No identity provider</div>
              <div className="text-xs text-ink-faint">This server authenticates with its own accounts. SSO is not available yet.</div>
            </div>
            <Badge tone="neutral">Not available</Badge>
          </Card>
        </Section>
        <Section title="What is enforced" description="The authentication rules the server applies to every account today.">
          <Facts rows={[
            { label: "Credentials", value: "Email and password, hashed with Argon2id" },
            { label: "Second factor", value: "TOTP authenticator app, enrolled per user" },
            { label: "Lockout", value: "Temporary lock after repeated failed sign-ins" },
            { label: "Sessions", value: "15-minute access tokens, silently refreshed; refresh tokens are single-use" },
            { label: "Password changes", value: "Sign every other session out" },
          ]} />
          <div className="flex justify-end">
            <Button variant="ghost" onClick={() => router.push("/settings/profile")}>Your sign-in settings</Button>
          </div>
        </Section>
      </div>
    </>
  );
}
