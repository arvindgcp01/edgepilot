"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { ApiError, signIn } from "@/lib/api/client";
import { useSession } from "@/components/session-provider";
import { Button, Card, ErrorNote, Input } from "@/components/ui";

export default function LoginPage() {
  const router = useRouter();
  const { principal, loading } = useSession();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [mfaCode, setMfaCode] = useState("");
  const [mfaRequired, setMfaRequired] = useState(false);
  const [organization, setOrganization] = useState("");
  const [orgRequired, setOrgRequired] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!loading && principal) router.replace("/");
  }, [loading, principal, router]);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      await signIn(email, password, {
        ...(mfaCode ? { mfa_code: mfaCode } : {}),
        ...(organization ? { organization } : {}),
      });
      router.replace("/");
    } catch (err) {
      // A missing second factor is not a failure to report as one: the password
      // was correct, and the form needs to grow a field rather than shout.
      if (err instanceof ApiError && err.status === 428) {
        setMfaRequired(true);
        setError(null);
      } else {
        setError(err);
        // The server never says which tenant a sign-in resolved to, so after a
        // failure the operator is offered the field that might fix it.
        if (err instanceof ApiError && err.status === 401) setOrgRequired(true);
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center px-4">
      <Card className="w-full max-w-sm">
        <h1 className="mb-1 text-lg font-semibold">EdgePilot</h1>
        <p className="mb-6 text-sm text-ink-muted">Sign in to manage your fleet.</p>

        <form onSubmit={submit} className="space-y-3">
          <label className="block text-sm">
            <span className="mb-1 block text-ink-muted">Email</span>
            <Input
              type="email" name="email" autoComplete="username" required autoFocus
              value={email} onChange={(e) => setEmail(e.target.value)}
            />
          </label>

          <label className="block text-sm">
            <span className="mb-1 block text-ink-muted">Password</span>
            <Input
              type="password" name="password" autoComplete="current-password" required
              value={password} onChange={(e) => setPassword(e.target.value)}
            />
          </label>

          {mfaRequired && (
            <label className="block text-sm">
              <span className="mb-1 block text-ink-muted">Authentication code</span>
              <Input
                inputMode="numeric" pattern="[0-9]{6}" maxLength={6} autoFocus required
                autoComplete="one-time-code"
                value={mfaCode} onChange={(e) => setMfaCode(e.target.value)}
              />
            </label>
          )}

          {orgRequired && (
            <label className="block text-sm">
              <span className="mb-1 block text-ink-muted">Organization</span>
              <Input
                placeholder="acme"
                value={organization} onChange={(e) => setOrganization(e.target.value)}
              />
              <span className="mt-1 block text-xs text-ink-faint">
                Only needed if this server hosts several organizations on one address.
              </span>
            </label>
          )}

          <ErrorNote error={error} />

          <Button type="submit" disabled={submitting} className="w-full">
            {submitting ? "Signing in…" : "Sign in"}
          </Button>
        </form>
      </Card>
    </main>
  );
}
