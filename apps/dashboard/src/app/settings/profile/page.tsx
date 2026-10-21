"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { useSession } from "@/components/session-provider";
import {
  Avatar, Badge, Button, Callout, Code, ConfirmDialog, ErrorNote, Facts, Field, Input, PageHeader, Section,
  useToast,
} from "@/components/ui";
import { ApiError, request, signOut } from "@/lib/api/client";
import { humanize } from "@/lib/labels";

/**
 * The operator's own account. Everything here talks to real endpoints —
 * /auth/change-password and /auth/logout-all exist today — which is why this
 * page has no "coming soon" anywhere on it.
 */
export default function ProfilePage() {
  const { principal } = useSession();
  const router = useRouter();
  const [confirmSignOutAll, setConfirmSignOutAll] = useState(false);
  const [busy, setBusy] = useState(false);

  if (!principal) return null;

  return (
    <>
      <PageHeader title="Profile" description="Your account, how you sign in, and where you are signed in." />
      <div className="space-y-4">
        <Section title="Account" description="Identity is managed by your organization. Contact an administrator to change your email or roles.">
          <div className="flex items-center gap-4">
            <Avatar name={principal.email} size="lg" />
            <div className="min-w-0">
              <div className="truncate text-sm font-medium text-ink">{principal.email}</div>
              <div className="mt-1 flex flex-wrap gap-1">
                {principal.roles.map((r) => <Badge key={r} tone={r === "admin" ? "accent" : "neutral"}>{humanize(r)}</Badge>)}
              </div>
            </div>
          </div>
          <Facts rows={[
            { label: "User ID", value: <Code>{principal.user_id}</Code> },
            { label: "Organization", value: <Code>{principal.org_id}</Code> },
            { label: "Permissions", value: `${principal.permissions.length} granted through ${principal.roles.length} ${principal.roles.length === 1 ? "role" : "roles"}` },
          ]} />
        </Section>

        <PasswordSection />

        <Section
          title="Sessions"
          description="Signing out everywhere revokes every refresh token in your family, including this one."
          footer={<Button variant="danger" onClick={() => setConfirmSignOutAll(true)}>Sign out of all sessions</Button>}
        >
          <p className="text-sm text-ink-muted">
            Use this if a device you were signed in on is lost, or you signed in somewhere you no longer trust. You will
            be returned to the sign-in page.
          </p>
        </Section>
      </div>

      <ConfirmDialog
        open={confirmSignOutAll}
        onClose={() => setConfirmSignOutAll(false)}
        title="Sign out of all sessions?"
        confirmLabel="Sign out everywhere"
        loading={busy}
        description="Every browser and device signed in as you is signed out immediately, including this one. Nothing else about your account changes."
        onConfirm={async () => {
          setBusy(true);
          try {
            await request<void>("/auth/logout-all", { method: "POST" }, false);
          } catch {
            // The token may already be gone; either way we end up signed out.
          } finally {
            await signOut();
            router.replace("/login");
          }
        }}
      />
    </>
  );
}

function PasswordSection() {
  const { push } = useToast();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const mismatch = confirm.length > 0 && next !== confirm;
  const ready = current.length > 0 && next.length >= 8 && next === confirm;

  return (
    <Section
      title="Password"
      description="At least 14 characters is the organization minimum. A passphrase of several unrelated words is easier to remember than a shorter password with symbols."
      footer={<Button variant="primary" type="submit" form="change-password" disabled={!ready} loading={busy}>Update password</Button>}
    >
      <form
        id="change-password"
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          setFieldErrors({});
          try {
            await request<void>("/auth/change-password", {
              method: "POST",
              body: JSON.stringify({ current_password: current, new_password: next }),
            });
            setCurrent(""); setNext(""); setConfirm("");
            push({ title: "Password updated", description: "Other sessions stay signed in until they expire.", tone: "success" });
          } catch (err) {
            if (err instanceof ApiError && Object.keys(err.fieldErrors).length) setFieldErrors(err.fieldErrors);
            else setError(err);
          } finally {
            setBusy(false);
          }
        }}
      >
        <Field label="Current password" htmlFor="pw-current" error={fieldErrors.current_password}>
          <Input id="pw-current" type="password" autoComplete="current-password" required value={current} onChange={(e) => setCurrent(e.target.value)} />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="New password" htmlFor="pw-new" error={fieldErrors.new_password}>
            <Input id="pw-new" type="password" autoComplete="new-password" required minLength={8} value={next} onChange={(e) => setNext(e.target.value)} />
          </Field>
          <Field label="Confirm new password" htmlFor="pw-confirm" error={mismatch ? "Passwords do not match." : undefined}>
            <Input id="pw-confirm" type="password" autoComplete="new-password" required value={confirm} aria-invalid={mismatch} onChange={(e) => setConfirm(e.target.value)} />
          </Field>
        </div>
        {next.length > 0 && next.length < 14 && (
          <Callout tone="warning">Shorter than the organization minimum of 14 characters — the server will refuse it.</Callout>
        )}
        <ErrorNote error={error} />
      </form>
    </Section>
  );
}
