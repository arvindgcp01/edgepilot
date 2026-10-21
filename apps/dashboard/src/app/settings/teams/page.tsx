"use client";

import { useRouter } from "next/navigation";
import { Button, Card, EmptyState, PageHeader } from "@/components/ui";

export default function TeamsPage() {
  const router = useRouter();
  return (
    <>
      <PageHeader title="Teams" description="Teams would group members for device access by site. Today every member's device access is the whole organization, scoped by role." />
      <Card padded={false}>
        <EmptyState
          icon="layers"
          title="Teams are not available on this server"
          description="There is no team model in the server yet. Roles decide what a member can do; site-scoped access will arrive with teams."
          action={<Button onClick={() => router.push("/settings/users")}>Manage users</Button>}
        />
      </Card>
    </>
  );
}
