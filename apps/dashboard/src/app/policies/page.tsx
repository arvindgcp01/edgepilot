"use client";

import { useRouter } from "next/navigation";
import { AppShell } from "@/components/app-shell";
import { Badge, Button, Card, EmptyState, PageHeader } from "@/components/ui";

/**
 * Policies are declared configuration applied to devices by label or site —
 * the layer above one-off commands. The route exists so the product's shape is
 * visible; the server has no policy engine yet, and the page says so rather
 * than showing a table of nothing.
 */
export default function PoliciesPage() {
  const router = useRouter();
  return (
    <AppShell>
      <PageHeader
        title={<span className="flex items-center gap-2">Policies <Badge tone="neutral">Preview</Badge></span>}
        description="Declarative configuration applied to devices by site or label — enrollment defaults, update rings, and access rules."
      />
      <Card padded={false}>
        <EmptyState
          icon="layers"
          title="Policies are not enabled on this server"
          description={
            <>
              Fleet-wide defaults currently live in Settings → Organization → Device management. When the policy
              engine ships, the same settings become targetable by site and label from here.
            </>
          }
          action={<Button onClick={() => router.push("/settings/organization")}>Open device management settings</Button>}
        />
      </Card>
    </AppShell>
  );
}
